import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../../api/client";
import type { Alert, AlertEvent } from "./text";

/**
 * How often the views of alerts ask again. The API has no push channel; a
 * page that is open and in front asks at this interval, and at once when
 * the person comes back to it.
 */
export const pollInterval = 5_000;

export type StatusFilter = Alert["status"] | "open" | "all";
export type SeverityFilter = Alert["severity"] | "any";

export interface AlertFilters {
  status: StatusFilter;
  severity: SeverityFilter;
  /** The name of a team, or "" for every team. */
  team: string;
}

export const alertKeys = {
  all: ["alerts"] as const,
  list: (filters: AlertFilters) => ["alerts", "list", filters] as const,
  detail: (id: string) => ["alerts", "detail", id] as const,
  timeline: (id: string) => ["alerts", "timeline", id] as const,
  group: (id: string) => ["alerts", "group", id] as const,
};

const pageSize = 50;

export function useAlerts(filters: AlertFilters) {
  return useInfiniteQuery({
    queryKey: alertKeys.list(filters),
    queryFn: ({ pageParam, signal }) =>
      unwrap(
        api.GET("/api/v1/alerts", {
          params: {
            query: {
              status: filters.status === "all" ? undefined : [filters.status],
              severity: filters.severity === "any" ? undefined : [filters.severity],
              team: filters.team === "" ? undefined : filters.team,
              limit: pageSize,
              cursor: pageParam,
            },
          },
          signal,
        }),
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor,
    refetchInterval: pollInterval,
  });
}

export function useAlert(id: string) {
  return useQuery({
    queryKey: alertKeys.detail(id),
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/alerts/{id}", { params: { path: { id } }, signal })),
    refetchInterval: pollInterval,
  });
}

export function useAlertGroup(id: string | undefined) {
  return useQuery({
    queryKey: alertKeys.group(id ?? ""),
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/alert-groups/{id}", { params: { path: { id: id ?? "" } }, signal })),
    enabled: id !== undefined,
    refetchInterval: pollInterval,
  });
}

// A timeline is read whole, page after page, up to a bound that no real
// alert comes near.
const timelinePages = 20;

export interface Timeline {
  events: AlertEvent[];
  /** False when the alert has more events than the page reads. */
  complete: boolean;
}

export function useTimeline(id: string) {
  return useQuery({
    queryKey: alertKeys.timeline(id),
    queryFn: async ({ signal }): Promise<Timeline> => {
      const events: AlertEvent[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < timelinePages; page++) {
        const answer = await unwrap(
          api.GET("/api/v1/alerts/{id}/timeline", { params: { path: { id }, query: { limit: 200, cursor } }, signal }),
        );
        events.push(...answer.events);
        cursor = answer.next_cursor;
        if (cursor === undefined) {
          return { events, complete: true };
        }
      }
      return { events, complete: false };
    },
    refetchInterval: pollInterval,
  });
}

export function useTeams() {
  return useQuery({
    queryKey: ["teams"],
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/teams", { signal })),
    staleTime: 60_000,
  });
}

export type Action = Schemas["Action"];

export interface ActionRequest {
  action: Action;
  /** For a snooze: how long, as the API writes durations. */
  duration?: string;
}

/** Acts on one alert, and makes every view of alerts ask again. */
export function useAlertAction(id: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ action, duration }: ActionRequest): Promise<Alert> => {
      const params = { path: { id } };
      switch (action) {
        case "acknowledge":
          return unwrap(api.POST("/api/v1/alerts/{id}/acknowledge", { params }));
        case "resolve":
          return unwrap(api.POST("/api/v1/alerts/{id}/resolve", { params }));
        case "snooze":
          return unwrap(api.POST("/api/v1/alerts/{id}/snooze", { params, body: { duration } }));
      }
    },
    onSuccess: (alert) => {
      queryClient.setQueryData(alertKeys.detail(id), alert);
    },
    // Also after a refusal: a 409 means the page was behind.
    onSettled: () => queryClient.invalidateQueries({ queryKey: alertKeys.all }),
  });
}
