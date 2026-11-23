import type { UseQueryResult } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ErrorNotice, Loading, PageHead, Provisioned, useTitle } from "../../app/ui";

/**
 * A page of what the configuration files define: its heading, the notice
 * that it is managed in the files, and the states of loading it. Such a
 * page has nothing that can be pressed to change anything.
 */
export function ConfigurationPage<T>({ title, query, children }: { title: string; query: UseQueryResult<T>; children: (data: T) => ReactNode }) {
  useTitle(title);
  return (
    <div className="page">
      <PageHead title={title} />
      <Provisioned />
      {query.data === undefined && query.isPending && <Loading />}
      {query.data === undefined && query.isError && <ErrorNotice error={query.error} onRetry={() => void query.refetch()} />}
      {query.data !== undefined && children(query.data)}
    </div>
  );
}

/** Name and value pairs such as labels, as text. */
export function Pairs({ pairs, operator = "=" }: { pairs: Record<string, string>; operator?: string }) {
  const names = Object.keys(pairs).sort();
  return (
    <span className="matchers">
      {names.map((name) => (
        <code key={name} className="matcher">
          {name}
          {operator}
          {JSON.stringify(pairs[name])}
        </code>
      ))}
    </span>
  );
}
