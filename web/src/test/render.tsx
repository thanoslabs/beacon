import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { MemoryRouter } from "react-router";
import { SessionProvider, type Me } from "../app/session";

export const ada: Me = {
  id: "7a0d0000-0000-4000-8000-000000000001",
  email: "ada@example.com",
  display_name: "Ada Example",
  role: "admin",
  session_expires_at: "2026-12-01T20:30:00Z",
};

export const grace: Me = { ...ada, id: "7a0d0000-0000-4000-8000-000000000002", email: "grace@example.com", display_name: "Grace Example", role: "member" };

export function newQueryClient(): QueryClient {
  // No retries and no waiting: a test sees an error at once.
  return new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
}

interface Options {
  /** The address the app is opened at. */
  at?: string;
  /** Who is signed in, for a page that is rendered without the App. */
  as?: Me;
}

/** Renders with what every part of the app expects around it. */
export function renderApp(ui: ReactElement, { at = "/", as }: Options = {}) {
  const queryClient = newQueryClient();
  const tree = as === undefined ? ui : <SessionProvider value={as}>{ui}</SessionProvider>;
  return {
    queryClient,
    ...render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={[at]}>{tree}</MemoryRouter>
      </QueryClientProvider>,
    ),
  };
}
