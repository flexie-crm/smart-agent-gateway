import { useEffect, useState } from 'react';
import { apiFetch } from '@lib/api';
import type { FileAttachment } from '@lib/chat-types';

// A file sent with a message, as the chat draws it: the thumbnail of a picture,
// and the label and colour of anything else. Used by the message a file was
// sent with and by the composer holding one about to be sent.

/**
 * The thumbnail of an attached image.
 *
 * It cannot be a plain <img src>: the file is behind the session, and a browser
 * does not put an Authorization header on an image request. So the bytes are
 * fetched the way every other call is and turned into an object URL.
 *
 * Which also makes it survive a reload, and that is the point rather than a
 * side effect. A preview made from the local File lives as long as the tab and
 * dies on refresh, so a reloaded conversation showed a question with a broken
 * picture above it. This one asks the server, which still has the file.
 */
export function AttachmentImage({ id, alt }: { id: string; alt: string }) {
  const [url, setUrl] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    let made: string | null = null;
    void (async () => {
      try {
        const res = await apiFetch(`/v1/chat/uploads/${id}`);
        if (!res.ok) return;
        const blob = await res.blob();
        if (!alive) return;
        made = URL.createObjectURL(blob);
        setUrl(made);
      } catch {
        // No thumbnail. The card still shows the name and the type, which is
        // most of what it was for.
      }
    })();
    return () => {
      alive = false;
      if (made) URL.revokeObjectURL(made);
    };
  }, [id]);

  if (!url) return <div className="size-full animate-pulse bg-muted" />;
  return <img src={url} alt={alt} className="size-full object-cover" />;
}

// ─── File type metadata for visual display ─────────────────────────────────
// Keyed on the file's own type, which the server already established from the
// name and matched a rule against. Guessing again from a MIME type the browser
// claimed would be a second, less reliable answer to a settled question.
const IMAGE_TYPES = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'heic', 'svg']);

export function isImageFile(file: FileAttachment): boolean {
  return IMAGE_TYPES.has(file.file_type);
}

export function getFileTypeInfo(file: FileAttachment) {
  const ext = file.file_type;
  if (ext === 'pdf') return { label: 'PDF', color: '#dc2626', bg: '#fef2f2' };
  if (ext === 'csv') return { label: 'CSV', color: '#16a34a', bg: '#f0fdf4' };
  if (ext === 'xlsx' || ext === 'xls') return { label: 'XLS', color: '#16a34a', bg: '#f0fdf4' };
  if (ext === 'docx' || ext === 'doc') return { label: 'DOC', color: '#2563eb', bg: '#eff6ff' };
  if (isImageFile(file)) return { label: ext.toUpperCase() || 'IMG', color: '#7c3aed', bg: '#f5f3ff' };
  return { label: ext.toUpperCase() || 'FILE', color: '#64748b', bg: '#f8fafc' };
}
