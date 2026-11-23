import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, unwrap, type Schemas } from "../../api/client";

export type Silence = Schemas["Silence"];
export type Matcher = Schemas["Matcher"];

export const silenceKeys = {
  all: ["silences"] as const,
  list: (expired: boolean) => ["silences", "list", expired] as const,
  preview: (matchers: Matcher[]) => ["silences", "preview", matchers] as const,
};

// Silences are short lists that the API reads whole and pages by position.
const pages = 5;

export function useSilences(expired: boolean) {
  return useQuery({
    queryKey: silenceKeys.list(expired),
    queryFn: async ({ signal }) => {
      const silences: Silence[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < pages; page++) {
        const answer = await unwrap(api.GET("/api/v1/silences", { params: { query: { expired: expired || undefined, limit: 200, cursor } }, signal }));
        silences.push(...answer.silences);
        cursor = answer.next_cursor;
        if (cursor === undefined) {
          break;
        }
      }
      return silences;
    },
    // A silence becomes active, and ends, without anyone doing anything.
    refetchInterval: 30_000,
  });
}

export interface NewSilence {
  matchers: Matcher[];
  /** How long, as the API writes durations, or undefined with endsAt. */
  duration?: string;
  starts_at?: string;
  ends_at?: string;
  comment: string;
}

export function useCreateSilence() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (silence: NewSilence) => unwrap(api.POST("/api/v1/silences", { body: silence })),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: silenceKeys.all }),
  });
}

export function useExpireSilence() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => unwrap(api.DELETE("/api/v1/silences/{id}", { params: { path: { id } } })),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: silenceKeys.all }),
  });
}

/** A value that follows another once it has stopped changing for a while. */
export function useSettled<T>(value: T, ms = 400): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setSettled(value), ms);
    return () => window.clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

/**
 * Which open alerts the matchers match now, as Beacon works it out. The app
 * never matches by itself: the rules of matching are the API's.
 */
export function usePreview(matchers: Matcher[], enabled: boolean) {
  return useQuery({
    queryKey: silenceKeys.preview(matchers),
    queryFn: ({ signal }) => unwrap(api.POST("/api/v1/silences/preview", { body: { matchers }, signal })),
    enabled,
    retry: false,
    staleTime: 0,
  });
}
