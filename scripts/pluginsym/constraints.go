package main

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"path/filepath"
	"slices"
	"strings"
)

func checkFileConstraint(path string, f *ast.File, hasCalls bool) []string {
	var expr constraint.Expr
	for _, group := range f.Comments {
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			if expr != nil || comment.Pos() > f.Package {
				return []string{fmt.Sprintf("%s: R4: misplaced or repeated //go:build directive", path)}
			}
			parsed, err := constraint.Parse(comment.Text)
			if err != nil {
				return []string{fmt.Sprintf("%s: R4: invalid build constraint: %v", path, err)}
			}
			expr = parsed
		}
	}
	families := map[string]bool{}
	negated := false
	collectFamilyTags(expr, false, families, &negated)
	if len(families) == 0 {
		if hasCalls {
			return []string{fmt.Sprintf("%s: R5: file without a family tag must not register decoders or wire factories", path)}
		}
		return nil
	}
	var violations []string
	if negated && hasCalls {
		violations = append(violations, fmt.Sprintf("%s: R3: negated family stub must not register decoders or wire factories", path))
	}
	if !exactFamilyConstraint(expr) || platformSuffix(path) {
		violations = append(violations, fmt.Sprintf(
			"%s: R4: family constraint must be exactly gobridge_<family> || gobridge_all (stub: !gobridge_<family> && !gobridge_all), with no GOOS/GOARCH filename suffix", path))
	}
	return violations
}

func collectFamilyTags(expr constraint.Expr, underNot bool, tags map[string]bool, negated *bool) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		if strings.HasPrefix(e.Tag, "gobridge_") {
			tags[e.Tag] = true
			if underNot {
				*negated = true
			}
		}
	case *constraint.NotExpr:
		collectFamilyTags(e.X, true, tags, negated)
	case *constraint.AndExpr:
		collectFamilyTags(e.X, underNot, tags, negated)
		collectFamilyTags(e.Y, underNot, tags, negated)
	case *constraint.OrExpr:
		collectFamilyTags(e.X, underNot, tags, negated)
		collectFamilyTags(e.Y, underNot, tags, negated)
	}
}

func exactFamilyConstraint(expr constraint.Expr) bool {
	var family, all constraint.Expr
	switch e := expr.(type) {
	case *constraint.OrExpr:
		family, all = e.X, e.Y
	case *constraint.AndExpr:
		left, okLeft := e.X.(*constraint.NotExpr)
		right, okRight := e.Y.(*constraint.NotExpr)
		if !okLeft || !okRight {
			return false
		}
		family, all = left.X, right.X
	default:
		return false
	}
	f, okFamily := family.(*constraint.TagExpr)
	a, okAll := all.(*constraint.TagExpr)
	return okFamily && okAll && a.Tag == "gobridge_all" &&
		strings.HasPrefix(f.Tag, "gobridge_") && f.Tag != "gobridge_all" && f.Tag != "gobridge_"
}

// Go's filename constraints are additional AND terms, even when go:build is exact.
func platformSuffix(path string) bool {
	name := strings.TrimSuffix(filepath.Base(path), ".go")
	_, suffix, found := strings.Cut(name, "_")
	if !found {
		return false
	}
	parts := strings.Split(suffix, "_")
	last := parts[len(parts)-1]
	return slices.Contains(strings.Fields("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos"), last) ||
		slices.Contains(strings.Fields("386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm"), last)
}
