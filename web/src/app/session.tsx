import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { createContext, useContext } from "react";
import { ApiError, api, unwrap, type Schemas } from "../api/client";

export type Me = Schemas["Me"];

export const meKey = ["me"] as const;

/** Who is signed in, or null when nobody is. */
export async function fetchMe(signal?: AbortSignal): Promise<Me | null> {
  try {
    return await unwrap(api.GET("/api/v1/me", { signal }));
  } catch (error) {
    if (error instanceof ApiError && error.status === 401) {
      return null;
    }
    throw error;
  }
}

export function useMeQuery() {
  return useQuery({ queryKey: meKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: 60_000 });
}

const SessionContext = createContext<Me | null>(null);

export const SessionProvider = SessionContext.Provider;

/** The signed-in person. Only pages behind the sign-in may ask. */
export function useSession(): Me {
  const me = useContext(SessionContext);
  if (me === null) {
    throw new Error("useSession was called outside the signed-in part of the app");
  }
  return me;
}

/**
 * Whether the person is an admin. It decides what the app offers; what a
 * person may do is decided by the API, which refuses a member either way.
 */
export function useIsAdmin(): boolean {
  return useSession().role === "admin";
}

/** Ends the session, and forgets everything the app has read with it. */
export function useSignOut(): () => Promise<void> {
  const queryClient = useQueryClient();
  return async () => {
    try {
      await unwrap(api.POST("/auth/logout"));
    } catch (error) {
      // Already signed out is as good as signed out.
      if (!(error instanceof ApiError && error.status === 401)) {
        throw error;
      }
    }
    forgetSession(queryClient);
  };
}

/**
 * Makes the app show the sign-in page, and drops everything that was read
 * with the session, so that it is not shown to whoever signs in next.
 */
export function forgetSession(queryClient: QueryClient): void {
  queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== meKey[0] });
  queryClient.setQueryData(meKey, null);
}

// Where a person wanted to go before they were sent to the identity
// provider. It is a path within the app, never a credential.
const returnToKey = "beacon.return_to";

export function rememberReturnTo(path: string): void {
  try {
    sessionStorage.setItem(returnToKey, path);
  } catch {
    // Without storage the person lands on the first page.
  }
}

/** The remembered path, once, and only if it is a path of this app. */
export function takeReturnTo(): string | null {
  try {
    const path = sessionStorage.getItem(returnToKey);
    sessionStorage.removeItem(returnToKey);
    if (path !== null && /^\/(?![/\\])/.test(path) && path !== "/") {
      return path;
    }
  } catch {
    // See above.
  }
  return null;
}
