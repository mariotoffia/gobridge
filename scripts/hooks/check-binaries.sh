#!/bin/bash
#
# check-binaries.sh - Pre-commit check for compiled Go binaries
#
# Detects compiled executables (Go binaries) in staged files and blocks the commit.
# Supports cross-platform detection: macOS (Mach-O), Linux (ELF), Windows (PE32).
#
# Exit codes:
#   0 - No binaries found (success)
#   1 - Binaries found (failure)
#

set -e

# Get list of staged files (Added, Copied, Modified)
staged_files=$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null || true)

if [ -z "$staged_files" ]; then
    exit 0
fi

# Check if a file is a compiled executable
is_executable_binary() {
    local file="$1"

    # Skip if file doesn't exist (could be deleted)
    [ ! -f "$file" ] && return 1

    # Get file type using the 'file' command
    local file_type
    file_type=$(file -b "$file" 2>/dev/null) || return 1

    # Match against executable patterns (not libraries, not object files)
    case "$file_type" in
        *"Mach-O"*executable*)  return 0 ;;  # macOS
        *"ELF"*executable*)     return 0 ;;  # Linux
        *"PE32"*executable*)    return 0 ;;  # Windows
        *"PE32+"*executable*)   return 0 ;;  # Windows 64-bit
    esac

    return 1
}

# Collect any binaries found
binaries_found=()
binary_types=()

while IFS= read -r file; do
    [ -z "$file" ] && continue

    if is_executable_binary "$file"; then
        binaries_found+=("$file")
        binary_types+=("$(file -b "$file" 2>/dev/null | cut -d',' -f1)")
    fi
done <<< "$staged_files"

# Report findings
if [ ${#binaries_found[@]} -gt 0 ]; then
    echo "" >&2
    echo "Compiled binary detected in staged files:" >&2
    echo "" >&2

    for i in "${!binaries_found[@]}"; do
        printf "   %-40s [%s]\n" "${binaries_found[$i]}" "${binary_types[$i]}" >&2
    done

    echo "" >&2
    echo "These appear to be Go binaries that should not be committed." >&2
    echo "Convention: built binaries must use the .out postfix (already in .gitignore)." >&2
    echo "" >&2
    echo "What to do next:" >&2
    echo "  * Unstage:     git reset HEAD <file>" >&2
    echo "  * Delete:      rm <file>" >&2
    echo "  * Bypass hook: git commit --no-verify  (use sparingly)" >&2
    echo "" >&2
    exit 1
fi

exit 0
