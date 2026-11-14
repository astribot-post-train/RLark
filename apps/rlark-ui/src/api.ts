export const AUTH_TOKEN_KEY = "rlark-auth-token";
export const AUTH_ROLE_KEY = "rlark-auth-role";
export const AUTH_EXPIRES_KEY = "rlark-auth-expires";
export const AUTH_USER_KEY = "rlark-auth-user";
export const UNAUTHORIZED_EVENT = "rlark:unauthorized";

export type AuthRole = "admin" | "user";

export interface LoginResponse {
  ok: boolean;
  role: AuthRole;
  token: string;
  expiresAt: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public body?: unknown,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

// A login lasts as long as its token (the Gateway's --jwt-token-ttl): it is
// kept in localStorage with the token's expiry, so it outlives the tab and is
// shared by every tab of the console, and it is gone once the token expires.
export function storeAuthSession(token: string, role: AuthRole, expiresAt: string, userName: string) {
  localStorage.setItem(AUTH_TOKEN_KEY, token);
  localStorage.setItem(AUTH_ROLE_KEY, role);
  localStorage.setItem(AUTH_EXPIRES_KEY, expiresAt);
  localStorage.setItem(AUTH_USER_KEY, userName);
}

export function clearAuthSession() {
  for (const key of [AUTH_TOKEN_KEY, AUTH_ROLE_KEY, AUTH_EXPIRES_KEY, AUTH_USER_KEY]) localStorage.removeItem(key);
}

export function hasAuthSession(role?: AuthRole) {
  const token = localStorage.getItem(AUTH_TOKEN_KEY);
  const storedRole = localStorage.getItem(AUTH_ROLE_KEY);
  const expiresAt = Date.parse(localStorage.getItem(AUTH_EXPIRES_KEY) ?? "");
  return Boolean(token && expiresAt > Date.now() && (!role || storedRole === role));
}

export function authUserName(fallback: string) {
  return localStorage.getItem(AUTH_USER_KEY) || fallback;
}

async function authenticatedFetch(
  input: RequestInfo | URL,
  init: RequestInit = {},
) {
  const headers = new Headers(init.headers);
  const token = localStorage.getItem(AUTH_TOKEN_KEY);
  if (token && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${token}`);
  }

  const response = await fetch(input, { ...init, headers });
  if (response.status === 401) {
    clearAuthSession();
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
  }
  return response;
}

// Reads the human-readable detail out of an error body. The Gateway returns
// `message` in its canonical error response; `error` is the legacy form that
// some handlers still emit.
export function errorMessage(body: unknown): string {
  if (!body || typeof body !== "object") return "";
  const record = body as Record<string, unknown>;
  if (typeof record.message === "string") return record.message;
  if (typeof record.error === "string") return record.error;
  return "";
}

async function responseError(response: Response) {
  const text = await response.text();
  let body: unknown = text;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    // Keep non-JSON error bodies as text.
  }
  const detail = errorMessage(body) || text;
  return new ApiError(
    response.status,
    detail ? `HTTP ${response.status}: ${detail}` : `HTTP ${response.status}`,
    body,
  );
}

export async function request(
  input: RequestInfo | URL,
  init: Omit<RequestInit, "body"> & { body?: BodyInit | object | null } = {},
) {
  const headers = new Headers(init.headers);
  let body = init.body;
  if (
    body != null &&
    typeof body === "object" &&
    !(body instanceof Blob) &&
    !(body instanceof FormData) &&
    !(body instanceof URLSearchParams) &&
    !(body instanceof ArrayBuffer) &&
    !ArrayBuffer.isView(body)
  ) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(body);
  }
  const response = await authenticatedFetch(input, {
    ...init,
    headers,
    body: body as BodyInit | null | undefined,
  });
  if (!response.ok) throw await responseError(response);
  return response;
}

export async function requestJson<T>(
  input: RequestInfo | URL,
  init: Omit<RequestInit, "body"> & { body?: BodyInit | object | null } = {},
) {
  const response = await request(input, init);
  return (await response.json()) as T;
}

export function withQuery(
  path: string,
  values: Record<string, string | number | boolean | undefined>,
) {
  const query = new URLSearchParams();
  Object.entries(values).forEach(([key, value]) => {
    if (value !== undefined && value !== "") query.set(key, String(value));
  });
  const suffix = query.toString();
  return suffix ? `${path}?${suffix}` : path;
}
