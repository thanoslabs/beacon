#!/usr/bin/env bash
# Fails when a committed YAML file cannot be parsed. Every document of a
# file with several is read. Needs python3 with PyYAML, which the hosted
# runners have; without it the check fails rather than pass unseen.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

git ls-files -z -- '*.yaml' '*.yml' | xargs -0 python3 -c '
import sys

try:
    import yaml
except ImportError:
    sys.exit("PyYAML is not installed: the YAML files were not checked")

bad = 0
for path in sys.argv[1:]:
    try:
        with open(path, encoding="utf-8") as f:
            for _ in yaml.safe_load_all(f):
                pass
    except (yaml.YAMLError, UnicodeDecodeError) as err:
        bad += 1
        line = getattr(getattr(err, "problem_mark", None), "line", 0) + 1
        problem = " ".join(str(err).split())
        print(f"::error file={path},line={line}::invalid YAML: {problem}")
if bad:
    sys.exit(f"{bad} of {len(sys.argv) - 1} YAML files are invalid")
if len(sys.argv) < 2:
    sys.exit("no YAML file was found: the check is looking in the wrong place")
print(f"{len(sys.argv) - 1} YAML files are valid")
'
