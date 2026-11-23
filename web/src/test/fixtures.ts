import type { Schemas } from "../api/client";

type Alert = Schemas["Alert"];
type AlertEvent = Schemas["Event"];

let next = 1;

/** An alert as the API answers one, firing unless told otherwise. */
export function alertOf(over: Partial<Alert> = {}): Alert {
  const n = next++;
  const status = over.status ?? "firing";
  return {
    id: `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`,
    group_id: `10000000-0000-4000-8000-${String(n).padStart(12, "0")}`,
    source: "default",
    fingerprint: `fp-${n}`,
    status,
    severity: "critical",
    labels: { alertname: "DiskFull", host: "db1", severity: "critical" },
    annotations: { summary: "Disk is full on db1" },
    links: {},
    team: "platform",
    service: null,
    routing_rule: null,
    started_at: "2026-12-01T10:12:00Z",
    last_received_at: "2026-12-01T10:17:00Z",
    acknowledged_at: status === "acknowledged" ? "2026-12-01T10:14:00Z" : null,
    snoozed_until: status === "snoozed" ? "2026-12-01T12:00:00Z" : null,
    resolved_at: status === "resolved" ? "2026-12-01T10:30:00Z" : null,
    actions: status === "resolved" ? [] : status === "firing" ? ["acknowledge", "snooze", "resolve"] : ["snooze", "resolve"],
    ...over,
  };
}

export function eventOf(type: string, over: Partial<AlertEvent> = {}): AlertEvent {
  return {
    id: next++,
    alert_id: null,
    group_id: "10000000-0000-4000-8000-000000000001",
    type,
    occurred_at: "2026-12-01T10:12:00Z",
    actor: { type: "system", id: null, name: null, provider: null, external_id: null },
    data: {},
    ...over,
  };
}

export const userActor = (name: string, provider: string | null = null): AlertEvent["actor"] => ({
  type: "user",
  id: "7a0d0000-0000-4000-8000-000000000001",
  name,
  provider,
  external_id: provider === null ? null : "U0123",
});

export const teams = {
  teams: [
    { name: "platform", display_name: "Platform", groups: [], members: [], schedules: [], provisioned: true },
    { name: "payments", display_name: "Payments", groups: [], members: [], schedules: [], provisioned: true },
  ],
};

type Schedule = Schemas["Schedule"];
type Shift = Schemas["Shift"];
type User = Schemas["User"];

export function userOf(email: string, name: string, over: Partial<User> = {}): User {
  return {
    id: `7a0d0000-0000-4000-8000-${String(next++).padStart(12, "0")}`,
    email,
    display_name: name,
    role: "member",
    disabled: false,
    created_at: "2026-11-02T09:00:00Z",
    last_sign_in_at: "2026-12-01T08:30:00Z",
    teams: ["platform"],
    ...over,
  };
}

export const users = { users: [userOf("ada@example.com", "Ada Example", { role: "admin" }), userOf("grace@example.com", "Grace Example")] };

export function scheduleOf(over: Partial<Schedule> = {}): Schedule {
  const name = over.name ?? "platform-primary";
  return {
    name,
    team: "platform",
    rotation: { time_zone: "Europe/London", members: ["ada@example.com", "grace@example.com"], shift_length: "1w", handoff_time: "09:00", handoff_day: "monday" },
    next_handoff: "2026-12-07T09:00:00Z",
    on_call: { schedule: name, user: "ada@example.com", since: "2026-11-30T09:00:00Z", until: "2026-12-07T09:00:00Z", rotation_user: "ada@example.com" },
    ...over,
  };
}

export function shiftOf(user: string, starts_at: string, ends_at: string, over: Partial<Shift> = {}): Shift {
  return { user, starts_at, ends_at, kind: "rotation", planned: false, ...over };
}
