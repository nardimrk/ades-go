package views

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every table header must be aligned like the cells of its column: a column
// of .num cells needs <th class="num">, a column of .center cells needs
// <th class="center"> (see the adesgo-ui skill). This checks every <table>
// in the templates against its first data row (group rows are skipped).
func TestTableHeadersAlignedWithCells(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil {
		t.Fatal(err)
	}
	var (
		tableRe = regexp.MustCompile(`(?s)<table.*?</table>`)
		thRe    = regexp.MustCompile(`<th(\s[^>]*)?>`)
		tdRe    = regexp.MustCompile(`<td(\s[^>]*)?>`)
		rowRe   = regexp.MustCompile(`(?s)<tr([^>]*)>(.*?)</tr>`)
		numRe   = regexp.MustCompile(`class="[^"]*\bnum\b`)
		cenRe   = regexp.MustCompile(`class="[^"]*\bcenter\b`)
	)
	align := func(attrs string) string {
		switch {
		case numRe.MatchString(attrs):
			return "num"
		case cenRe.MatchString(attrs):
			return "center"
		}
		return "left"
	}
	checked := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range tableRe.FindAllIndex(src, -1) {
			table := string(src[loc[0]:loc[1]])
			line := strings.Count(string(src[:loc[0]]), "\n") + 1
			_, body, ok := strings.Cut(table, "<tbody>")
			if !ok {
				continue
			}
			var cells []string
			for _, r := range rowRe.FindAllStringSubmatch(body, -1) {
				if !strings.Contains(r[1], "group-row") {
					for _, m := range tdRe.FindAllStringSubmatch(r[2], -1) {
						cells = append(cells, align(m[1]))
					}
					break
				}
			}
			heads := thRe.FindAllStringSubmatch(table, -1)
			for i, c := range cells {
				if i >= len(heads) {
					break
				}
				if h := align(heads[i][1]); h != c {
					t.Errorf("%s:%d column %d: header is %s-aligned but cells are %s-aligned", f, line, i+1, h, c)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no tables found")
	}
}
