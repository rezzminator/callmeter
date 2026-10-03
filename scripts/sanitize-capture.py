#!/usr/bin/env python3
"""sanitize-capture.py --map FROM=TO ... --terms FILE IN OUT

Turns a raw Claude Code capture (hook payloads, transcripts, tool-results)
into a fixture that may be committed to this public repo.

IN and OUT are both files or both directories; a directory is copied whole,
its relative file paths rewritten by the same maps as the bytes.

Every file, every byte, JSON-unaware:
  1. each FROM is replaced by its TO, longest FROM first, in one pass (a TO is
     never rewritten again by a shorter map);
  2. every email address except noreply@anthropic.com becomes user@example.com
     (a JSON escape letter before the address, as in a newline-then-address,
     is kept);
  3. the result is judged: a line still matching a term from FILE (the leak
     terms format of scripts/leak-check.sh: one lowercase regex per line,
     `#` comments, `!ignore-token <regex>`; case-insensitive), `/Users/`, `/home/`,
     `/private/`, `/tmp/callmeter/` or `claude-501` is a leftover.

Any leftover refuses the whole run: exit 1, every leftover named as
`path:line` on stderr (never the line: it holds what must not be printed), and
nothing written. The machine paths to rewrite travel as --map arguments; this
script holds no private string.
"""

import argparse
import os
import re
import sys

KEPT_EMAIL = "noreply@anthropic.com"
NEUTRAL_EMAIL = "user@example.com"
EMAIL = re.compile(rb"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
LEFTOVERS = [rb"/Users/", rb"/home/", rb"/private/", rb"/tmp/callmeter/", rb"claude-501"]


def parse_maps(pairs):
    maps = []
    for pair in pairs:
        if "=" not in pair:
            sys.exit(f"sanitize-capture: --map {pair!r} is not FROM=TO")
        source, target = pair.split("=", 1)
        if not source:
            sys.exit(f"sanitize-capture: --map {pair!r} has an empty FROM")
        maps.append((source.encode(), target.encode()))
    return maps


def read_terms(path):
    terms, ignore = [], []
    try:
        with open(path, encoding="utf-8") as handle:
            lines = handle.read().splitlines()
    except OSError as err:
        sys.exit(f"sanitize-capture: read terms file {path}: {err}")
    for line in lines:
        line = line.rstrip("\r")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line.startswith("!ignore-token "):
            ignore.append(re.compile(line[len("!ignore-token "):].encode(), re.IGNORECASE))
            continue
        if line.startswith("!"):
            sys.exit(f"sanitize-capture: unknown directive in {path}")
        terms.append(re.compile(line.encode(), re.IGNORECASE))
    if not terms:
        # No denylist finds nothing, which is not the same as finding no leak.
        sys.exit(f"sanitize-capture: terms file {path} has zero usable terms")
    return terms, ignore


def rewriter(maps):
    if not maps:
        return lambda data: data
    ordered = sorted(maps, key=lambda pair: len(pair[0]), reverse=True)
    table = dict(ordered)
    pattern = re.compile(b"|".join(re.escape(source) for source, _ in ordered))
    return lambda data: pattern.sub(lambda match: table[match.group(0)], data)


def neutral_email(match):
    address = match.group(0)
    if address.lower() == KEPT_EMAIL.encode():
        return address
    start = match.start()
    if start > 0 and match.string[start - 1 : start] == b"\\":
        # \nuser@… inside a JSON string: the n belongs to the escape.
        return address[:1] + NEUTRAL_EMAIL.encode()
    return NEUTRAL_EMAIL.encode()


def leftovers(data, terms, ignore):
    """The 1-based numbers of the lines still holding a forbidden string."""
    found = []
    for number, line in enumerate(data.split(b"\n"), start=1):
        judged = line
        for token in ignore:
            judged = token.sub(b"<ignored>", judged)
        if any(bad in line for bad in LEFTOVERS) or any(term.search(judged) for term in terms):
            found.append(number)
    return found


def collect(source):
    """(relative path, absolute path) of every file under source, sorted."""
    if os.path.isfile(source):
        return [("", source)]
    if not os.path.isdir(source):
        sys.exit(f"sanitize-capture: {source} is neither a file nor a directory")
    files = []
    for root, _, names in os.walk(source):
        for name in names:
            path = os.path.join(root, name)
            files.append((os.path.relpath(path, source), path))
    return sorted(files)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--map", action="append", default=[], metavar="FROM=TO")
    parser.add_argument("--terms", required=True, metavar="FILE")
    parser.add_argument("source", metavar="IN")
    parser.add_argument("target", metavar="OUT")
    args = parser.parse_args()

    rewrite = rewriter(parse_maps(args.map))
    terms, ignore = read_terms(args.terms)

    outputs, refused = [], []
    for relative, path in collect(args.source):
        with open(path, "rb") as handle:
            data = handle.read()
        data = EMAIL.sub(neutral_email, rewrite(data))
        renamed = rewrite(relative.encode()).decode()
        target = os.path.join(args.target, renamed) if renamed else args.target
        for number in leftovers(data, terms, ignore):
            refused.append(f"{target}:{number}")
        if leftovers(renamed.encode(), terms, ignore):
            refused.append(f"{target} (its relative path)")
        outputs.append((target, data))

    if refused:
        for where in refused:
            print(f"sanitize-capture: leftover at {where}", file=sys.stderr)
        print(f"sanitize-capture: REFUSED, {len(refused)} leftover(s); nothing written", file=sys.stderr)
        return 1
    for target, data in outputs:
        os.makedirs(os.path.dirname(target) or ".", exist_ok=True)
        with open(target, "wb") as handle:
            handle.write(data)
    print(f"sanitize-capture: wrote {len(outputs)} file(s) to {args.target}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
