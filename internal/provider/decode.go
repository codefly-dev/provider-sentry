package provider

import (
	"sort"
	"strconv"
	"strings"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/provider/sdk"
)

// elementPath is the concrete selector prefix for one top-level bare-array
// element, matching how the response filter names wildcard matches ("$[N]").
func elementPath(index int) string {
	return "$[" + strconv.Itoa(index) + "]"
}

// filteredResponse is the flat map of concrete selector paths to safe values the
// host forwarded. Wildcard selectors resolve to one entry per matched element
// (e.g. "$[0].id"), so a bare-array list projects to many keys sharing an index
// prefix. No secret is ever among them: the manifest suppresses the legacy
// `secret` and `dsn.secret` before the broker forwards anything.
type filteredResponse map[string]*providerv0.PublicValue

func decodeFiltered(response *providerv0.ExecuteRequestResponse) (filteredResponse, error) {
	fields, err := sdk.DecodeFilteredResponse(response)
	if err != nil {
		return nil, err
	}
	return filteredResponse(fields), nil
}

func (f filteredResponse) string(path string) string {
	if value, ok := f[path]; ok {
		if s, ok := publicString(value); ok {
			return s
		}
	}
	return ""
}

func (f filteredResponse) bool(path string) bool {
	if value, ok := f[path]; ok {
		if _, isBool := value.GetKind().(*providerv0.PublicValue_BoolValue); isBool {
			return value.GetBoolValue()
		}
	}
	return false
}

// rootIndices returns the sorted element indices present in a top-level bare
// array ("$[N]..."), keyed off each element's forwarded id.
func (f filteredResponse) rootIndices() []int {
	seen := map[int]struct{}{}
	for key := range f {
		if index, ok := elementIndex(key, "$", ".id"); ok {
			seen[index] = struct{}{}
		}
	}
	indices := make([]int, 0, len(seen))
	for index := range seen {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

// elementIndex parses a concrete wildcard path of the form "<prefix>[N]<suffix>"
// and returns N.
func elementIndex(key, prefix, suffix string) (int, bool) {
	rest, ok := strings.CutPrefix(key, prefix+"[")
	if !ok {
		return 0, false
	}
	digits, tail, ok := strings.Cut(rest, "]")
	if !ok || tail != suffix || digits == "" {
		return 0, false
	}
	index := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		index = index*10 + int(r-'0')
	}
	return index, true
}
