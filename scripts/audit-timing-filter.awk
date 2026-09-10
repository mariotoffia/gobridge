# Filters `rg -n` hits against audit/test-timing-allowlist.txt.
#
# An allowlist keyed by FILE AND LINE NUMBER breaks on any edit above an allowed
# line: the entry stops matching and the audit fails on a file nobody touched at
# that line. That happened three times in one afternoon. The key here is the
# file plus the CODE, which is what the entry is actually about — an allowed
# `time.Sleep` that moves down a file is the same allowed sleep.
#
# Moving a sleep to a different FILE, or changing the line itself, still
# requires a new entry, which is the point: the exemption is for that call, not
# for a path.
#
# Input and output are `rg -n` lines (`path:line:code`), so the report keeps the
# exact file:line a developer needs.

function key(line,   k) {
    k = line
    # Drop the first `:<digits>:` field — the line number rg inserted after the
    # path. Everything after it is the source line, colons and all.
    sub(/:[0-9]+:/, ":", k)
    return k
}

BEGIN {
    allowlist = "audit/test-timing-allowlist.txt"
    while ((getline entry < allowlist) > 0) {
        if (entry ~ /^[[:space:]]*$/ || entry ~ /^[[:space:]]*#/) {
            continue
        }
        allowed[key(entry)] = 1
    }
    close(allowlist)
}

!(key($0) in allowed) { print }
