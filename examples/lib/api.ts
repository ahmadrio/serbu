// Helper yang di-import oleh 02-api-login.ts (contoh import file lokal).

export const BASE_URL: string = __ENV.BASE_URL || 'http://localhost:8787';

export function jsonHeaders(token?: string): Record<string, string> {
  const h: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'application/json',
  };
  if (token) h.Authorization = `Bearer ${token}`;
  return h;
}
