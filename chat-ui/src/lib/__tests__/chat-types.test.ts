import { describe, it, expect } from 'vitest';
import {
  isConfirmMessage,
  isAssistantMessage,
  isUserMessage,
  type ChatMessage,
} from '../chat-types';

// ═══════════════════════════════════════════════════════════════════════════
// isConfirmMessage
// ═══════════════════════════════════════════════════════════════════════════

describe('isConfirmMessage', () => {
  it('returns true for confirm role with confirmation data', () => {
    const msg: ChatMessage = {
      id: '1',
      role: 'confirm',
      content: '',
      confirmation: {
        token: 'abc',
        title: 'X',
        description: 'Y',
        severity: 'read_only',
        status: 'pending',
      },
    };
    expect(isConfirmMessage(msg)).toBe(true);
  });

  it('returns false for confirm role without confirmation data', () => {
    const msg: ChatMessage = { id: '1', role: 'confirm', content: '' };
    expect(isConfirmMessage(msg)).toBe(false);
  });

  it('returns false for assistant role', () => {
    const msg: ChatMessage = { id: '1', role: 'assistant', content: 'hi' };
    expect(isConfirmMessage(msg)).toBe(false);
  });

  it('returns false for user role', () => {
    const msg: ChatMessage = { id: '1', role: 'user', content: 'hello' };
    expect(isConfirmMessage(msg)).toBe(false);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// isAssistantMessage
// ═══════════════════════════════════════════════════════════════════════════

describe('isAssistantMessage', () => {
  it('returns true for assistant role', () => {
    const msg: ChatMessage = { id: '1', role: 'assistant', content: 'reply' };
    expect(isAssistantMessage(msg)).toBe(true);
  });

  it('returns false for user role', () => {
    const msg: ChatMessage = { id: '1', role: 'user', content: 'q' };
    expect(isAssistantMessage(msg)).toBe(false);
  });

  it('returns false for confirm role', () => {
    const msg: ChatMessage = { id: '1', role: 'confirm', content: '' };
    expect(isAssistantMessage(msg)).toBe(false);
  });
});

// ═══════════════════════════════════════════════════════════════════════════
// isUserMessage
// ═══════════════════════════════════════════════════════════════════════════

describe('isUserMessage', () => {
  it('returns true for user role', () => {
    const msg: ChatMessage = { id: '1', role: 'user', content: 'question' };
    expect(isUserMessage(msg)).toBe(true);
  });

  it('returns false for assistant role', () => {
    const msg: ChatMessage = { id: '1', role: 'assistant', content: 'answer' };
    expect(isUserMessage(msg)).toBe(false);
  });
});
