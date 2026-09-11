import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/**
 * Resolves a config value that may be a plain value, a sync factory function,
 * or an async factory function. Used for any `value | () => value | () =>
 * Promise<value>` config field — currently `extraData` and `token` — so host
 * pages can compute per-request context (current record id, fresh tokens,
 * etc.) without having to keep `window.FlexieAiConfig` in sync manually.
 */
export async function resolveDynamic(input: unknown): Promise<unknown> {
  if (typeof input === 'function') {
    return await (input as () => unknown | Promise<unknown>)();
  }
  return input;
}

export function t(
  key: string,
  lang?: Record<string, string>,
  fallback?: string,
  params?: Record<string, string | number>
): string {
  const template = lang?.[key] ?? fallback ?? key;
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (_, k) => {
    const v = params[k];
    return v !== undefined ? String(v) : `{${k}}`;
  });
}