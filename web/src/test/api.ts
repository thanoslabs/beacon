// A stand-in for Beacon's API in component tests: answers by method and
// path, and remembers what was asked.
import { vi } from "vitest";

// A handler is a JSON body for a 200, a Response, or a function that makes
// either from the request.
type Handler = unknown;

export interface Call {
  method: string;
  path: string;
  search: URLSearchParams;
  body: unknown;
  headers: Headers;
}

export interface FakeApi {
  calls: Call[];
  /** The calls of one route, such as "POST /auth/logout". */
  callsTo(route: string): Call[];
  /** Replaces or adds answers while a test runs. */
  on(routes: Record<string, Handler>): void;
}

/** A status with a JSON body, for answers that are not 200. */
export function status(code: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status: code,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
  });
}

/**
 * Makes fetch answer from routes, keyed "METHOD /path" without the query.
 * A value is the JSON body of a 200; a Response is sent as it is. A route
 * that is not listed fails the test with a 500 that names it.
 */
export function fakeApi(routes: Record<string, Handler>): FakeApi {
  const table = new Map(Object.entries(routes));
  const calls: Call[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    const url = new URL(request.url);
    const route = `${request.method} ${url.pathname}`;
    const text = await request.clone().text();
    calls.push({
      method: request.method,
      path: url.pathname,
      search: url.searchParams,
      body: text === "" ? undefined : (JSON.parse(text) as unknown),
      headers: request.headers,
    });
    if (!table.has(route)) {
      return status(500, { error: `the test has no answer for ${route}` });
    }
    const handler = table.get(route);
    const answer = typeof handler === "function" ? await (handler as (request: Request) => unknown)(request) : handler;
    return answer instanceof Response ? answer : status(200, answer);
  });
  return {
    calls,
    callsTo: (route) => calls.filter((call) => `${call.method} ${call.path}` === route),
    on: (more) => {
      for (const [route, handler] of Object.entries(more)) {
        table.set(route, handler);
      }
    },
  };
}

export const unauthenticated = () => status(401, { error: "authentication is required: sign in at /auth/login" });
