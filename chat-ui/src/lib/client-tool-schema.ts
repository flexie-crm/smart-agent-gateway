// lib/client-tool-schema.ts
// ─── Minimal JSON Schema validator for client-tool arguments ────────────────
// Just the subset the LLM is allowed to use in tool parameter schemas:
//   - type: "object" with properties / required
//   - per-property type: string | number | integer | boolean | array | object | null
//   - enum (any type)
//
// Anything else is treated as "no constraint" — the dispatcher trusts the host
// handler to handle unexpected shapes. Pulling in a real JSON-Schema library
// would balloon the lib bundle for very limited additional safety.

export type ClientToolParameters = {
  type?: 'object';
  properties?: Record<string, ClientToolPropSchema>;
  required?: string[];
  additionalProperties?: boolean;
};

export type ClientToolPropSchema = {
  type?: 'string' | 'number' | 'integer' | 'boolean' | 'array' | 'object' | 'null' | string;
  enum?: any[];
  items?: ClientToolPropSchema;
};

export type ValidationResult = { ok: true } | { ok: false; error: string };

export function validateClientToolArgs(args: unknown, schema: ClientToolParameters | undefined): ValidationResult {
  if (!schema || typeof schema !== 'object') return { ok: true };

  if (schema.type === 'object' || schema.properties || schema.required) {
    if (args === null || typeof args !== 'object' || Array.isArray(args)) {
      return { ok: false, error: 'args must be an object' };
    }
    const argsObj = args as Record<string, unknown>;

    if (Array.isArray(schema.required)) {
      for (const key of schema.required) {
        if (!(key in argsObj)) {
          return { ok: false, error: `missing required arg "${key}"` };
        }
      }
    }

    if (schema.properties && typeof schema.properties === 'object') {
      for (const [key, propSchema] of Object.entries(schema.properties)) {
        if (!(key in argsObj)) continue;
        const inner = validateValue(argsObj[key], propSchema, key);
        if (!inner.ok) return inner;
      }
    }
  }

  return { ok: true };
}

function validateValue(value: unknown, propSchema: ClientToolPropSchema | undefined, path: string): ValidationResult {
  if (!propSchema) return { ok: true };

  if (Array.isArray(propSchema.enum) && propSchema.enum.length > 0) {
    if (!propSchema.enum.includes(value)) {
      return { ok: false, error: `arg "${path}" is not in the allowed enum` };
    }
  }

  const expected = propSchema.type;
  if (typeof expected !== 'string') return { ok: true };

  if (value === null) {
    if (expected === 'null') return { ok: true };
    return { ok: false, error: `arg "${path}" must be of type ${expected}` };
  }

  switch (expected) {
    case 'string':
      if (typeof value !== 'string') return { ok: false, error: `arg "${path}" must be a string` };
      break;
    case 'number':
      if (typeof value !== 'number' || Number.isNaN(value)) return { ok: false, error: `arg "${path}" must be a number` };
      break;
    case 'integer':
      if (typeof value !== 'number' || !Number.isFinite(value) || !Number.isInteger(value)) {
        return { ok: false, error: `arg "${path}" must be an integer` };
      }
      break;
    case 'boolean':
      if (typeof value !== 'boolean') return { ok: false, error: `arg "${path}" must be a boolean` };
      break;
    case 'array':
      if (!Array.isArray(value)) return { ok: false, error: `arg "${path}" must be an array` };
      if (propSchema.items) {
        for (let i = 0; i < value.length; i++) {
          const inner = validateValue(value[i], propSchema.items, `${path}[${i}]`);
          if (!inner.ok) return inner;
        }
      }
      break;
    case 'object':
      if (typeof value !== 'object' || Array.isArray(value)) {
        return { ok: false, error: `arg "${path}" must be an object` };
      }
      break;
    default:
      // Unknown type → no constraint
      break;
  }

  return { ok: true };
}
