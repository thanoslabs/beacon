import type { Matcher } from "./queries";

/** Matchers as they are written in Prometheus: label, operator, value. */
export function MatcherList({ matchers }: { matchers: Matcher[] }) {
  return (
    <ul className="matchers" aria-label="Matchers">
      {matchers.map((matcher, index) => (
        <li key={index}>
          <code className="matcher">
            {matcher.label}
            {matcher.operator}
            {JSON.stringify(matcher.value)}
          </code>
        </li>
      ))}
    </ul>
  );
}
