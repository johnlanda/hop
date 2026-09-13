package internal

import (
	"cmp"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// guideIssue is one guide-coverage, navigation or link failure.
type guideIssue struct {
	guide string // module-relative path of the AGENTS.md concerned
	link  string // link target, when the finding is link-specific
	rule  string
}

func (g guideIssue) String() string {
	return fmt.Sprintf("%s: %s: %s", g.guide, cmp.Or(g.link, "-"), g.rule)
}

// compareGuideIssues orders issues by guide, link and rule.
func compareGuideIssues(a, b guideIssue) int {
	return cmp.Or(strings.Compare(a.guide, b.guide), strings.Compare(a.link, b.link), strings.Compare(a.rule, b.rule))
}

// guideReport is the outcome of checking one tree's guides.
type guideReport struct {
	packages []string // module-relative package directories found by the source walk
	guides   []string // module-relative paths of every AGENTS.md outside excluded trees
	issues   []guideIssue
}

// guideDirectories maps every directory that must carry a guide to the
// immediate children that must carry one too: the root, every package
// directory and every grouping directory between a package and the root.
func guideDirectories(packages []string) map[string][]string {
	dirs := map[string][]string{".": nil}
	for _, pkg := range packages {
		for dir := pkg; dir != "."; dir = parentDirectory(dir) {
			dirs[dir] = nil
		}
	}
	for dir := range dirs {
		if dir != "." {
			parent := parentDirectory(dir)
			dirs[parent] = append(dirs[parent], dir)
		}
	}
	for dir := range dirs {
		slices.Sort(dirs[dir])
	}
	return dirs
}

// parentDirectory returns the parent of a slash-separated module-relative
// directory, "." for a top-level directory.
func parentDirectory(dir string) string {
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[:i]
	}
	return "."
}

// guidePath returns the module-relative path of a directory's AGENTS.md.
func guidePath(dir string) string {
	if dir == "." {
		return "AGENTS.md"
	}
	return dir + "/AGENTS.md"
}

// linkSchemePattern matches a link that starts with a URL scheme.
var linkSchemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// markdownDefinitionPattern matches a reference definition at the start of
// a line, capturing its label and target.
var markdownDefinitionPattern = regexp.MustCompile(`(?m)^ {0,3}\[([^\[\]]+)\]:[ \t]*<?([^\s>]+)>?`)

// listItemPattern matches the start of a list item, capturing its leading
// spaces, its marker and the spaces after the marker.
var listItemPattern = regexp.MustCompile(`^( *)([-+*]|\d{1,9}[.)])( +|$)`)

// markdownDocument is the link-relevant content of one guide.
type markdownDocument struct {
	links       []string // targets of rendered links: inline links, images and reference uses resolved through their definitions, in document order
	definitions []string // targets of reference definitions; validated as links but rendering nothing, so they never count as navigation
	undefined   []string // normalized labels of full or collapsed reference uses without a definition, sorted
}

// parseMarkdown extracts the links of a guide. Fenced and indented code
// blocks, inline code spans and backslash escapes are read literally first,
// so the brackets of a Go type such as map[string][]Task never form a link.
// Link text may contain balanced brackets. Full [text][label] and collapsed
// [label][] uses resolve through the definition table or are reported as
// undefined; a shortcut [label] is a link only when a definition carries its
// label and is plain text otherwise. A label may not itself contain brackets.
func parseMarkdown(content string) markdownDocument {
	text := stripMarkdownCode(content)
	var doc markdownDocument
	defined := map[string]string{}
	for _, match := range markdownDefinitionPattern.FindAllStringSubmatch(text, -1) {
		doc.definitions = append(doc.definitions, match[2])
		if label := referenceLabel(match[1]); defined[label] == "" {
			defined[label] = match[2]
		}
	}
	text = markdownDefinitionPattern.ReplaceAllString(text, "")
	for i := 0; i < len(text); {
		if text[i] != '[' {
			i++
			continue
		}
		end := bracketSpan(text, i)
		if end < 0 {
			i++
			continue
		}
		label := text[i+1 : end-1]
		if target, next, ok := inlineDestination(text, end); ok {
			if target != "" {
				doc.links = append(doc.links, target)
			}
			i = next
			continue
		}
		if end < len(text) && text[end] == '[' {
			if refEnd := bracketSpan(text, end); refEnd >= 0 {
				refLabel := cmp.Or(text[end+1:refEnd-1], label)
				if validLabel(refLabel) {
					if target := defined[referenceLabel(refLabel)]; target != "" {
						doc.links = append(doc.links, target)
					} else if normalized := referenceLabel(refLabel); !slices.Contains(doc.undefined, normalized) {
						doc.undefined = append(doc.undefined, normalized)
					}
				}
				i = refEnd
				continue
			}
		}
		if validLabel(label) {
			if target := defined[referenceLabel(label)]; target != "" {
				doc.links = append(doc.links, target)
			}
		}
		i = end
	}
	slices.Sort(doc.undefined)
	return doc
}

// bracketSpan returns the index just past the ']' that balances the '[' at
// open, or -1 when the brackets do not balance before the text ends.
func bracketSpan(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// inlineDestination parses the "(destination "title")" part of an inline
// link starting at index at and returns the destination and the index just
// past the closing parenthesis. A destination in angle brackets stays on
// one line; a bare destination ends at whitespace or an unbalanced ')'.
func inlineDestination(text string, at int) (target string, next int, ok bool) {
	if at >= len(text) || text[at] != '(' {
		return "", 0, false
	}
	j := skipWhitespace(text, at+1)
	if j < len(text) && text[j] == '<' {
		closing := strings.IndexByte(text[j:], '>')
		if closing < 0 || strings.Contains(text[j:j+closing], "\n") {
			return "", 0, false
		}
		target = text[j+1 : j+closing]
		j += closing + 1
	} else {
		start := j
		depth := 0
	scan:
		for ; j < len(text); j++ {
			switch text[j] {
			case ' ', '\t', '\n':
				break scan
			case '(':
				depth++
			case ')':
				if depth == 0 {
					break scan
				}
				depth--
			}
		}
		target = text[start:j]
	}
	j = skipWhitespace(text, j)
	if j < len(text) {
		if closer, titled := map[byte]byte{'"': '"', '\'': '\'', '(': ')'}[text[j]]; titled {
			closing := strings.IndexByte(text[j+1:], closer)
			if closing < 0 {
				return "", 0, false
			}
			j = skipWhitespace(text, j+1+closing+1)
		}
	}
	if j < len(text) && text[j] == ')' {
		return target, j + 1, true
	}
	return "", 0, false
}

// skipWhitespace returns the index of the first byte at or after i that is
// not a space, tab or line break.
func skipWhitespace(text string, i int) int {
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
		i++
	}
	return i
}

// validLabel reports whether a reference label is well formed: non-blank
// and free of brackets.
func validLabel(label string) bool {
	return strings.TrimSpace(label) != "" && !strings.ContainsAny(label, "[]")
}

// referenceLabel normalizes a reference label the way Markdown matches it:
// case-insensitively, with runs of whitespace collapsed.
func referenceLabel(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// stripMarkdownCode blanks what Markdown renders literally: fenced code
// blocks, indented code blocks, inline code spans and backslash-escaped
// punctuation. Line breaks are kept so that definitions stay anchored to
// line starts, and lines holding only spaces or tabs become empty so they
// bound paragraphs. An indented line opens a code block only after a blank
// line and only when indented four columns beyond the content of the
// enclosing list item, so a wrapped list item and a list item's continuation
// paragraph keep their text.
func stripMarkdownCode(content string) string {
	var prose strings.Builder
	fence := ""        // the run of backticks or tildes that opened the current fenced block
	afterBlank := true // the previous line was blank, so an indented line starts a code block
	indented := false  // inside an indented code block
	contentIndent := 0 // column where the current list item's content starts; 0 outside a list
	for line := range strings.Lines(content) {
		text, newline := strings.CutSuffix(line, "\n")
		text = strings.TrimSuffix(text, "\r")
		trimmed := strings.TrimLeft(text, " \t")
		switch {
		case fence != "":
			if closesFence(trimmed, fence) {
				fence = ""
			}
		case trimmed == "":
			afterBlank = true
			indented = false
		case fenceRun(trimmed) != "":
			fence = fenceRun(trimmed)
			afterBlank = false
			indented = false
		case (afterBlank || indented) && leadingIndent(text) >= contentIndent+4:
			indented = true
		default:
			indented = false
			afterBlank = false
			if itemIndent, ok := listContentIndent(text); ok {
				contentIndent = itemIndent
			} else if leadingIndent(text) < contentIndent {
				contentIndent = 0
			}
			prose.WriteString(text)
		}
		if newline {
			prose.WriteByte('\n')
		}
	}
	return blankInlineCode(prose.String())
}

// leadingIndent returns the column at which a line's content starts, with a
// tab advancing to the next multiple of four.
func leadingIndent(text string) int {
	column := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ':
			column++
		case '\t':
			column += 4 - column%4
		default:
			return column
		}
	}
	return column
}

// listContentIndent returns the column at which the content of a list item
// line starts and whether the line starts a list item. More than four spaces
// after the marker count as one, the rest being content.
func listContentIndent(text string) (int, bool) {
	match := listItemPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	spaces := len(match[3])
	if spaces == 0 || spaces > 4 {
		spaces = 1
	}
	return len(match[1]) + len(match[2]) + spaces, true
}

// fenceRun returns the run of at least three backticks or tildes that
// starts a trimmed line, or "" when the line is not a fence.
func fenceRun(trimmed string) string {
	for _, marker := range []byte{'`', '~'} {
		run := 0
		for run < len(trimmed) && trimmed[run] == marker {
			run++
		}
		if run >= 3 {
			return trimmed[:run]
		}
	}
	return ""
}

// closesFence reports whether a trimmed line closes the fenced block opened
// by fence: the same marker at least as long, followed by nothing.
func closesFence(trimmed, fence string) bool {
	run := fenceRun(trimmed)
	return run != "" && run[0] == fence[0] && len(run) >= len(fence) && strings.TrimSpace(trimmed[len(run):]) == ""
}

// blankInlineCode blanks backtick code spans and backslash-escaped
// punctuation. A span closes at the next backtick run of the same length and
// cannot cross a blank line, which stripMarkdownCode has reduced to an empty
// line even when it held spaces or tabs; an unmatched run is literal text.
func blankInlineCode(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '\\' && i+1 < len(text) && isASCIIPunctuation(text[i+1]):
			out.WriteString("  ")
			i += 2
		case c == '`':
			run := backtickRun(text, i)
			end := closingBacktickRun(text, i+run, run)
			if end < 0 {
				out.WriteString(text[i : i+run])
				i += run
				continue
			}
			for _, r := range text[i:end] {
				if r == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
			}
			i = end
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// backtickRun returns the number of consecutive backticks starting at i.
func backtickRun(text string, i int) int {
	run := 0
	for i+run < len(text) && text[i+run] == '`' {
		run++
	}
	return run
}

// closingBacktickRun returns the index just past the run of exactly length
// backticks that closes a span opened before from, or -1 when a blank line
// or the end of text comes first.
func closingBacktickRun(text string, from, length int) int {
	for j := from; j < len(text); {
		if strings.HasPrefix(text[j:], "\n\n") {
			return -1
		}
		if text[j] != '`' {
			j++
			continue
		}
		run := backtickRun(text, j)
		if run == length {
			return j + run
		}
		j += run
	}
	return -1
}

// isASCIIPunctuation reports whether a backslash before b is an escape.
func isASCIIPunctuation(b byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", b) >= 0
}

// linkResolution is the outcome of resolving one link written in a guide.
type linkResolution struct {
	target  string // module-relative path the link names, when it is local
	skip    bool   // the link is not verified: an external URL, an in-page anchor or a path into an excluded tree
	problem string // why the link is wrong regardless of the target's existence
}

// resolveLink resolves a link written in guide to a module-relative path.
// Links into excluded trees are skipped because reference checkouts may be
// absent from a clone.
func resolveLink(guide, link string) linkResolution {
	if strings.HasPrefix(link, "#") || linkSchemePattern.MatchString(link) {
		return linkResolution{skip: true}
	}
	target, _, _ := strings.Cut(link, "#")
	if strings.HasPrefix(target, "/") {
		return linkResolution{problem: "absolute link; use a path relative to the guide"}
	}
	target = path.Join(path.Dir(guide), target)
	if target == ".." || strings.HasPrefix(target, "../") {
		return linkResolution{problem: "link leaves the repository"}
	}
	if excludedComponent(target) != "" {
		return linkResolution{target: target, skip: true}
	}
	return linkResolution{target: target}
}

// findGuides returns the module-relative path of every AGENTS.md below root
// outside the excluded trees, sorted.
func findGuides(root string) ([]string, error) {
	var guides []string
	err := filepath.WalkDir(root, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if fullPath != root && isExcludedDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "AGENTS.md" {
			return nil
		}
		rel, err := filepath.Rel(root, fullPath)
		if err != nil {
			return err
		}
		guides = append(guides, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(guides)
	return guides, nil
}

// checkGuides inventories the Go packages below root and reports every
// directory that must carry an AGENTS.md but does not, every guide whose
// rendered links do not reach an immediate child guide, every local link or
// reference definition in any AGENTS.md that does not resolve, and every
// reference-style use without a definition.
func checkGuides(root string) (*guideReport, error) {
	tree, err := walkSources(root)
	if err != nil {
		return nil, err
	}
	guides, err := findGuides(root)
	if err != nil {
		return nil, err
	}
	documents := make(map[string]markdownDocument, len(guides))
	for _, guide := range guides {
		data, err := readTreeFile(root, guide)
		if err != nil {
			return nil, err
		}
		documents[guide] = parseMarkdown(string(data))
	}
	packages := slices.Sorted(maps.Keys(tree.packages))
	var issues []guideIssue
	dirs := guideDirectories(packages)
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		guide := guidePath(dir)
		doc, ok := documents[guide]
		if !ok {
			issues = append(issues, guideIssue{guide: guide, rule: missingGuideRule(dir, tree)})
			continue
		}
		for _, child := range dirs[dir] {
			want := guidePath(child)
			linked := slices.ContainsFunc(doc.links, func(link string) bool { return resolveLink(guide, link).target == want })
			if !linked {
				issues = append(issues, guideIssue{guide: guide, link: want, rule: "guide does not link its immediate child guide"})
			}
		}
	}
	for _, guide := range guides {
		doc := documents[guide]
		for _, label := range doc.undefined {
			issues = append(issues, guideIssue{guide: guide, link: "[" + label + "]", rule: "reference link has no definition"})
		}
		for _, link := range slices.Concat(doc.links, doc.definitions) {
			resolved := resolveLink(guide, link)
			switch {
			case resolved.problem != "":
				issues = append(issues, guideIssue{guide: guide, link: link, rule: resolved.problem})
			case resolved.skip:
				continue
			default:
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resolved.target))); err != nil {
					issues = append(issues, guideIssue{guide: guide, link: link, rule: "broken local link"})
				}
			}
		}
	}
	slices.SortFunc(issues, compareGuideIssues)
	return &guideReport{packages: packages, guides: guides, issues: slices.Compact(issues)}, nil
}

// missingGuideRule names what kind of guide a directory lacks.
func missingGuideRule(dir string, tree *sourceTree) string {
	if dir == "." {
		return "missing root guide"
	}
	if _, ok := tree.packages[dir]; ok {
		return "missing package guide"
	}
	return "missing index guide for a grouping directory"
}

// TestGuidesRealTree checks the HOP tree itself. The inventory must contain
// the packages and guides known to exist, so an enumeration that found
// nothing cannot pass.
func TestGuidesRealTree(t *testing.T) {
	root := moduleRoot(t)

	result, err := checkGuides(root)
	if err != nil {
		t.Fatalf("checking guides in %s: %v", root, err)
	}

	for _, want := range []string{"cmd/hop", "internal"} {
		if !slices.Contains(result.packages, want) {
			t.Errorf("the package inventory does not contain %s; it found %v", want, result.packages)
		}
	}
	for _, want := range []string{"AGENTS.md", "cmd/AGENTS.md", "cmd/hop/AGENTS.md", "internal/AGENTS.md"} {
		if !slices.Contains(result.guides, want) {
			t.Errorf("the guide inventory does not contain %s; it found %v", want, result.guides)
		}
	}
	for _, issue := range result.issues {
		t.Error(issue)
	}
}

func TestGuidesDirectoryMap(t *testing.T) {
	cases := []struct {
		name     string
		packages []string
		want     map[string][]string
	}{
		{
			name:     "root package alone",
			packages: []string{"."},
			want:     map[string][]string{".": nil},
		},
		{
			name:     "nested package adds every grouping ancestor",
			packages: []string{"internal/domain/run"},
			want: map[string][]string{
				".":                   {"internal"},
				"internal":            {"internal/domain"},
				"internal/domain":     {"internal/domain/run"},
				"internal/domain/run": nil,
			},
		},
		{
			name:     "siblings are sorted under one parent",
			packages: []string{"internal", "cmd/hop", "cmd/alpha"},
			want: map[string][]string{
				".":         {"cmd", "internal"},
				"cmd":       {"cmd/alpha", "cmd/hop"},
				"cmd/alpha": nil,
				"cmd/hop":   nil,
				"internal":  nil,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := guideDirectories(tc.packages)

			if !maps.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("guideDirectories(%v) = %v, want %v", tc.packages, got, tc.want)
			}
		})
	}
}

func TestGuidesMarkdownLinks(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    markdownDocument
	}{
		{
			name:    "inline link and image",
			content: "See [main](main.go) and ![diagram](flow.svg \"title\").\n",
			want:    markdownDocument{links: []string{"main.go", "flow.svg"}},
		},
		{
			name:    "full, collapsed and shortcut uses resolve through the definition",
			content: "[Internal][internal] and [internal][] and [Internal].\n\n[internal]: internal/AGENTS.md\n",
			want:    markdownDocument{links: []string{"internal/AGENTS.md", "internal/AGENTS.md", "internal/AGENTS.md"}, definitions: []string{"internal/AGENTS.md"}},
		},
		{
			name:    "definition alone renders nothing",
			content: "[docs]: <docs/AGENTS.md> \"Docs\"\n",
			want:    markdownDocument{definitions: []string{"docs/AGENTS.md"}},
		},
		{
			name:    "full and collapsed uses without a definition",
			content: "[Missing][missing] and [Other Thing][]\n",
			want:    markdownDocument{undefined: []string{"missing", "other thing"}},
		},
		{
			name:    "shortcut without a definition is plain text",
			content: "The [importer] field names the package.\n",
			want:    markdownDocument{},
		},
		{
			name:    "labels match case-insensitively with collapsed whitespace",
			content: "[x][Package  Guide]\n\n[package guide]: AGENTS.md\n",
			want:    markdownDocument{links: []string{"AGENTS.md"}, definitions: []string{"AGENTS.md"}},
		},
		{
			name:    "inline code spans are literal",
			content: "Keep `map[string][]Task` and `` a ` b [c][d] `` in prose.\n",
			want:    markdownDocument{},
		},
		{
			name:    "fenced code block is literal",
			content: "```go\nvar tasks map[string][]Task\n[fake][nowhere]\n[nowhere]: missing.md\n```\n\n[real](main.go)\n",
			want:    markdownDocument{links: []string{"main.go"}},
		},
		{
			name:    "tilde fence is literal",
			content: "~~~\n[fake][nowhere]\n~~~\n",
			want:    markdownDocument{},
		},
		{
			name:    "indented code block is literal",
			content: "Example:\n\n    tasks := map[string][]Task{}\n    [fake][nowhere]\n\n[real](main.go)\n",
			want:    markdownDocument{links: []string{"main.go"}},
		},
		{
			name:    "wrapped list item is prose",
			content: "- [Commands](cmd/AGENTS.md) and a long line that\n    wraps to [Internal](internal/AGENTS.md)\n",
			want:    markdownDocument{links: []string{"cmd/AGENTS.md", "internal/AGENTS.md"}},
		},
		{
			name:    "escaped brackets are literal",
			content: "Use \\[label\\] literally, \\[a][b], \\[c]\\[d] and \\[not a link](main.go).\n",
			want:    markdownDocument{},
		},
		{
			name:    "unmatched backtick is literal text",
			content: "A stray ` here and [Real][real].\n\n[real]: real.md\n",
			want:    markdownDocument{links: []string{"real.md"}, definitions: []string{"real.md"}},
		},
		{
			name:    "nested brackets in link text",
			content: "[Commands [CLI]](cmd/AGENTS.md) and ![shot [1]](shot.png)\n",
			want:    markdownDocument{links: []string{"cmd/AGENTS.md", "shot.png"}},
		},
		{
			name:    "nested brackets in reference text",
			content: "[Commands [CLI]][cmd] and [Internal [checkers]][]\n\n[cmd]: cmd/AGENTS.md\n",
			want:    markdownDocument{links: []string{"cmd/AGENTS.md"}, definitions: []string{"cmd/AGENTS.md"}},
		},
		{
			name:    "unbalanced bracket is plain text",
			content: "A [N byte array and [Real](real.md)\n",
			want:    markdownDocument{links: []string{"real.md"}},
		},
		{
			name:    "destination with a title and parentheses",
			content: "[a](docs/a.md \"Title\") [b](docs/b(1).md) [c](<docs/c d.md> 'T') [d]( docs/d.md )\n",
			want:    markdownDocument{links: []string{"docs/a.md", "docs/b(1).md", "docs/c d.md", "docs/d.md"}},
		},
		{
			name:    "list continuation paragraph after a blank line is prose",
			content: "- Package guides:\n\n    [Commands](cmd/AGENTS.md) and [Internal](internal/AGENTS.md)\n",
			want:    markdownDocument{links: []string{"cmd/AGENTS.md", "internal/AGENTS.md"}},
		},
		{
			name:    "indented code inside a list item is literal",
			content: "- Example:\n\n      tasks := map[string][]Task{}\n      [fake][nowhere]\n\n[real](main.go)\n",
			want:    markdownDocument{links: []string{"main.go"}},
		},
		{
			name:    "whitespace-only line bounds a paragraph for code spans",
			content: "`unfinished\n   \n[Commands](cmd/AGENTS.md) closing`\n \t \n[Internal](internal/AGENTS.md)\n",
			want:    markdownDocument{links: []string{"cmd/AGENTS.md", "internal/AGENTS.md"}},
		},
		{
			name:    "whitespace-only line bounds an indented code block",
			content: "text\n   \n    [fake][nowhere]\n",
			want:    markdownDocument{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMarkdown(tc.content)

			if !slices.Equal(got.links, tc.want.links) {
				t.Errorf("links = %q, want %q", got.links, tc.want.links)
			}
			if !slices.Equal(got.definitions, tc.want.definitions) {
				t.Errorf("definitions = %q, want %q", got.definitions, tc.want.definitions)
			}
			if !slices.Equal(got.undefined, tc.want.undefined) {
				t.Errorf("undefined = %q, want %q", got.undefined, tc.want.undefined)
			}
		})
	}
}

func TestGuidesLinkResolution(t *testing.T) {
	cases := []struct {
		name  string
		guide string
		link  string
		want  linkResolution
	}{
		{name: "sibling file", guide: "cmd/hop/AGENTS.md", link: "main.go", want: linkResolution{target: "cmd/hop/main.go"}},
		{name: "parent guide with anchor", guide: "cmd/hop/AGENTS.md", link: "../AGENTS.md#purpose", want: linkResolution{target: "cmd/AGENTS.md"}},
		{name: "root guide child", guide: "AGENTS.md", link: "docs/AGENTS.md", want: linkResolution{target: "docs/AGENTS.md"}},
		{name: "in-page anchor", guide: "AGENTS.md", link: "#map", want: linkResolution{skip: true}},
		{name: "external URL", guide: "AGENTS.md", link: "https://example.com/x", want: linkResolution{skip: true}},
		{name: "mail link", guide: "AGENTS.md", link: "mailto:x@example.com", want: linkResolution{skip: true}},
		{name: "reference checkout is skipped", guide: "docs/plan/AGENTS.md", link: "../../repos/herdr/README.md", want: linkResolution{target: "repos/herdr/README.md", skip: true}},
		{name: "absolute path", guide: "AGENTS.md", link: "/Users/someone/file.md", want: linkResolution{problem: "absolute link; use a path relative to the guide"}},
		{name: "escaping the repository", guide: "cmd/AGENTS.md", link: "../../other/README.md", want: linkResolution{problem: "link leaves the repository"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveLink(tc.guide, tc.link)

			if got != tc.want {
				t.Errorf("resolveLink(%q, %q) = %+v, want %+v", tc.guide, tc.link, got, tc.want)
			}
		})
	}
}

// guideFixtureFiles is a complete synthetic tree: a root guide, a grouping
// directory with an index guide, a test-only package, leaf guides with
// resolvable links, and Go files inside excluded trees that need no guide.
func guideFixtureFiles() map[string]string {
	return map[string]string{
		"go.mod":                        "module example.com/hop\n\ngo 1.24\n",
		"AGENTS.md":                     "# root\n\n- [Commands](cmd/AGENTS.md)\n- [Internal](internal/AGENTS.md)\n- [Docs](docs/AGENTS.md)\n- [Upstream](https://example.com/upstream)\n",
		"docs/AGENTS.md":                "# docs\n\n[Root](../AGENTS.md#purpose)\n",
		"cmd/AGENTS.md":                 "# cmd\n\n| Directory | Guide |\n| --- | --- |\n| `hop/` | [cmd/hop](hop/AGENTS.md) |\n",
		"cmd/hop/main.go":               "package main\n\nfunc main() {}\n",
		"cmd/hop/AGENTS.md":             "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n",
		"internal/arch_test.go":         "package internal\n",
		"internal/AGENTS.md":            "# internal\n\n[Domain](domain/AGENTS.md)\n",
		"internal/domain/AGENTS.md":     "# internal/domain\n\n[Run](run/AGENTS.md)\n",
		"internal/domain/run/run.go":    "package run\n",
		"internal/domain/run/AGENTS.md": "# internal/domain/run\n\n[run.go](run.go) and a [reference](../../../repos/ref/README.md).\n",
		"repos/ref/ref.go":              "package ref\n",
		"testdata/sample/sample.go":     "package sample\n",
		".worktrees/copy/go.mod":        "module example.com/copy\n\ngo 1.24\n",
		".worktrees/copy/main.go":       "package main\n\nfunc main() {}\n",
	}
}

func TestGuidesFixtures(t *testing.T) {
	cases := []struct {
		name       string
		files      map[string]string // overrides of guideFixtureFiles; empty content deletes
		wantIssues []string          // every issue must match one of these, and each must be matched
		wantError  string
	}{
		{
			name: "complete tree passes",
		},
		{
			name:  "package without a guide",
			files: map[string]string{"internal/domain/run/AGENTS.md": ""},
			wantIssues: []string{
				"internal/domain/run/AGENTS.md: -: missing package guide",
				"internal/domain/AGENTS.md: run/AGENTS.md: broken local link",
			},
		},
		{
			name:  "test-only package without a guide",
			files: map[string]string{"internal/AGENTS.md": ""},
			wantIssues: []string{
				"internal/AGENTS.md: -: missing package guide",
				"AGENTS.md: internal/AGENTS.md: broken local link",
			},
		},
		{
			name:  "grouping directory without an index guide",
			files: map[string]string{"internal/domain/AGENTS.md": ""},
			wantIssues: []string{
				"internal/domain/AGENTS.md: -: missing index guide for a grouping directory",
				"internal/AGENTS.md: domain/AGENTS.md: broken local link",
			},
		},
		{
			name:       "root guide not linking a child",
			files:      map[string]string{"AGENTS.md": "# root\n\n- [Commands](cmd/AGENTS.md)\n"},
			wantIssues: []string{"AGENTS.md: internal/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "index guide not linking a child",
			files:      map[string]string{"internal/domain/AGENTS.md": "# internal/domain\n\nNo map here.\n"},
			wantIssues: []string{"internal/domain/AGENTS.md: internal/domain/run/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "package guide not linking a nested package",
			files:      map[string]string{"internal/AGENTS.md": "# internal\n\nNo map here.\n"},
			wantIssues: []string{"internal/AGENTS.md: internal/domain/AGENTS.md: guide does not link its immediate child guide"},
		},
		{
			name:       "broken local link",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[wire.go](wire.go) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: wire.go: broken local link"},
		},
		{
			name:       "broken reference-style link",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go), the [parent](../AGENTS.md) and [Missing][missing].\n\n[missing]: nonexistent.md\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: nonexistent.md: broken local link"},
		},
		{
			name:  "child links written as reference uses backed by definitions",
			files: map[string]string{"AGENTS.md": "# root\n\n- [Commands][cmd]\n- [Internal][]\n- [docs]\n\n[cmd]: cmd/AGENTS.md\n[internal]: internal/AGENTS.md\n[docs]: docs/AGENTS.md\n"},
		},
		{
			name:  "only unused reference definitions do not navigate",
			files: map[string]string{"AGENTS.md": "# Root\n\n[cmd]: cmd/AGENTS.md\n[internal]: internal/AGENTS.md\n"},
			wantIssues: []string{
				"AGENTS.md: cmd/AGENTS.md: guide does not link its immediate child guide",
				"AGENTS.md: internal/AGENTS.md: guide does not link its immediate child guide",
			},
		},
		{
			name:       "unused definition with a broken target",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n\n[stale]: nowhere.md\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: nowhere.md: broken local link"},
		},
		{
			name:       "reference link without a definition",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go), the [parent](../AGENTS.md) and [Wiring][wiring].\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: [wiring]: reference link has no definition"},
		},
		{
			name:  "inline code span with brackets is prose",
			files: map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n\nThe store keeps `map[string][]Task` per run.\n"},
		},
		{
			name:  "fenced Go block with brackets is not a link",
			files: map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n\n```go\nvar tasks map[string][]Task\n[fake link][nowhere]\n[nowhere]: missing.md\n```\n"},
		},
		{
			name:  "indented code block with brackets is not a link",
			files: map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n\n    tasks := map[string][]Task{}\n    [fake link][nowhere]\n"},
		},
		{
			name:  "escaped brackets are literal",
			files: map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go) and the [parent](../AGENTS.md).\n\nWrite \\[label\\], \\[x][y] and \\[text](nowhere.md) literally.\n"},
		},
		{
			name:  "nested brackets in inline link label",
			files: map[string]string{"AGENTS.md": "# Root\n\n[Commands [CLI]](cmd/AGENTS.md)\n[Internal](internal/AGENTS.md)\n"},
		},
		{
			name:       "broken target behind a nested-bracket label",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](main.go), the [parent](../AGENTS.md) and [Wiring [planned]](wire.go).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: wire.go: broken local link"},
		},
		{
			name:  "list continuation paragraph after blank line",
			files: map[string]string{"AGENTS.md": "# Root\n\n- Package guides:\n\n    [Commands](cmd/AGENTS.md) and [Internal](internal/AGENTS.md)\n"},
		},
		{
			name:  "indented code block inside a list item is not a link",
			files: map[string]string{"AGENTS.md": "# Root\n\n- [Commands](cmd/AGENTS.md) and [Internal](internal/AGENTS.md), for example:\n\n      tasks := map[string][]Task{}\n      [fake link][nowhere]\n"},
		},
		{
			name:  "backticks separated by empty line",
			files: map[string]string{"AGENTS.md": "# Root\n\n`unfinished\n\n[Commands](cmd/AGENTS.md) closing`\n\n[Internal](internal/AGENTS.md)\n"},
		},
		{
			name:  "backticks separated by spaces-only blank line",
			files: map[string]string{"AGENTS.md": "# Root\n\n`unfinished\n   \n[Commands](cmd/AGENTS.md) closing`\n\n[Internal](internal/AGENTS.md)\n"},
		},
		{
			name:  "table with backticked Go symbols",
			files: map[string]string{"AGENTS.md": "# Root\n\n| Package | Symbols |\n| --- | --- |\n| [Commands](cmd/AGENTS.md) | `run`, `map[string][]Task` |\n| [Internal](internal/AGENTS.md) | `sourceTree`, `[N]byte` |\n"},
		},
		{
			name:       "absolute link",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[main.go](/cmd/hop/main.go) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: /cmd/hop/main.go: absolute link"},
		},
		{
			name:       "link leaving the repository",
			files:      map[string]string{"cmd/hop/AGENTS.md": "# cmd/hop\n\n[secret](../../../outside.md) and the [parent](../AGENTS.md).\n"},
			wantIssues: []string{"cmd/hop/AGENTS.md: ../../../outside.md: link leaves the repository"},
		},
		{
			name: "tree without packages",
			files: map[string]string{
				"cmd/hop/main.go": "", "internal/arch_test.go": "", "internal/domain/run/run.go": "",
			},
			wantError: "no Go package found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := guideFixtureFiles()
			for rel, content := range tc.files {
				if content == "" {
					delete(files, rel)
					continue
				}
				files[rel] = content
			}
			root := t.TempDir()
			writeFiles(t, root, files)

			result, err := checkGuides(root)

			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkGuides: %v", err)
			}
			rendered := make([]string, len(result.issues))
			for i, issue := range result.issues {
				rendered[i] = issue.String()
			}
			for _, fragment := range tc.wantIssues {
				if !slices.ContainsFunc(rendered, func(s string) bool { return strings.Contains(s, fragment) }) {
					t.Errorf("no issue contains %q; got:\n%s", fragment, strings.Join(rendered, "\n"))
				}
			}
			for _, s := range rendered {
				if !slices.ContainsFunc(tc.wantIssues, func(fragment string) bool { return strings.Contains(s, fragment) }) {
					t.Errorf("unexpected issue: %s", s)
				}
			}
		})
	}
}
