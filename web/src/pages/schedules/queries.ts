import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../../api/client";

export type Schedule = Schemas["Schedule"];
export type OnCall = Schemas["OnCall"];
export type User = Schemas["User"];

/** Who is on call changes at a handoff or with an override, not by the second. */
export const onCallInterval = 30_000;

export const scheduleKeys = {
  all: ["schedules"] as const,
  list: ["schedules", "list"] as const,
  detail: (name: string) => ["schedules", "detail", name] as const,
  shifts: (name: string, from: string, to: string) => ["schedules", "shifts", name, from, to] as const,
  overrides: (name: string) => ["schedules", "overrides", name] as const,
};

export function useSchedules() {
  return useQuery({
    queryKey: scheduleKeys.list,
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/schedules", { signal })),
    refetchInterval: onCallInterval,
  });
}

export function useSchedule(name: string) {
  return useQuery({
    queryKey: scheduleKeys.detail(name),
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/schedules/{name}", { params: { path: { name } }, signal })),
    refetchInterval: onCallInterval,
  });
}

export function useShifts(name: string, from: Date, to: Date) {
  const range = { from: from.toISOString(), to: to.toISOString() };
  return useQuery({
    queryKey: scheduleKeys.shifts(name, range.from, range.to),
    queryFn: ({ signal }) => unwrap(api.GET("/api/v1/schedules/{name}/shifts", { params: { path: { name }, query: range }, signal })),
    refetchInterval: onCallInterval,
    // The weeks that were shown stay while the next ones load.
    placeholderData: (previous) => previous,
  });
}

// Users are read whole, to name people by more than their address. An
// install has as many as an organisation has engineers.
const userPages = 25;

export function useUsers() {
  return useQuery({
    queryKey: ["users"],
    queryFn: async ({ signal }) => {
      const users: User[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < userPages; page++) {
        const answer = await unwrap(api.GET("/api/v1/users", { params: { query: { limit: 200, cursor } }, signal }));
        users.push(...answer.users);
        cursor = answer.next_cursor;
        if (cursor === undefined) {
          break;
        }
      }
      return users;
    },
    staleTime: 60_000,
  });
}

/** Names a person by their address: their name when Beacon knows it. */
export function usePersonName(): (email: string) => string {
  const users = useUsers();
  return (email) => users.data?.find((user) => user.email === email)?.display_name || email;
}
