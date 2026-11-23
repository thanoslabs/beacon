import { useQuery } from "@tanstack/react-query";
import { ApiError, api, unwrap, type Schemas } from "../../api/client";

export type EscalationPolicy = Schemas["EscalationPolicy"];
export type Routing = Schemas["Routing"];
export type Team = Schemas["Team"];
export type Source = Schemas["Source"];
export type Integration = Schemas["Integration"];

// What the files define changes only at a reload.
const staleTime = 30_000;

export function usePolicies() {
  return useQuery({ queryKey: ["escalation-policies"], queryFn: ({ signal }) => unwrap(api.GET("/api/v1/escalation-policies", { signal })), staleTime });
}

/** The routing block, or null when the configuration has none. */
export function useRouting() {
  return useQuery({
    queryKey: ["routing"],
    queryFn: async ({ signal }): Promise<Routing | null> => {
      try {
        return await unwrap(api.GET("/api/v1/routing", { signal }));
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) {
          return null;
        }
        throw error;
      }
    },
    staleTime,
  });
}

export function useSources() {
  return useQuery({ queryKey: ["sources"], queryFn: ({ signal }) => unwrap(api.GET("/api/v1/sources", { signal })), staleTime });
}

export function useIntegrations() {
  return useQuery({ queryKey: ["integrations"], queryFn: ({ signal }) => unwrap(api.GET("/api/v1/integrations", { signal })), staleTime });
}
