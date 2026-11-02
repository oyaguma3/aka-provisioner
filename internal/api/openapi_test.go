package api

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestAllowlistMatchesOpenAPI は、下流ごとに「許す口 ＋ 通さない口」が、下流の OpenAPI の口とぴったり同じであることを確かめる
// （段階 3-1 の U3）。下流に口が増えたり消えたりしたら落ちるので、allowlist か denylist を直す。
// 下流の OpenAPI のパスは環境変数で与える（CI の契約の試験のジョブで、固定のコミットの下流から渡す）。なければ飛ばす。
func TestAllowlistMatchesOpenAPI(t *testing.T) {
	files := map[kind]string{
		kindVecgen: os.Getenv("PROVISIONER_TEST_VECGEN_OPENAPI"),
		kindRadius: os.Getenv("PROVISIONER_TEST_RADIUS_OPENAPI"),
		kindCA:     os.Getenv("PROVISIONER_TEST_CA_OPENAPI"),
		kindEdge:   os.Getenv("PROVISIONER_TEST_EDGE_OPENAPI"),
	}
	for _, name := range kinds {
		t.Run(string(name), func(t *testing.T) {
			path := files[name]
			if path == "" {
				t.Skip("the OpenAPI file of the downstream is not given")
			}
			got, err := openAPIRoutes(path)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, rt := range append(slices.Clone(allowlist[name]), denylist[name]...) {
				want = append(want, rt.method+" "+rt.path)
			}
			slices.Sort(want)
			for _, r := range got {
				if !slices.Contains(want, r) {
					t.Errorf("%s has %s, but it is neither allowed nor denied", name, r)
				}
			}
			for _, r := range want {
				if !slices.Contains(got, r) {
					t.Errorf("%s does not have %s", name, r)
				}
			}
		})
	}
}

// TestOpenAPIRoutes は、: を含むパスと、口の下の parameters を読めることを確かめる。
func TestOpenAPIRoutes(t *testing.T) {
	path := t.TempDir() + "/openapi.yaml"
	if err := os.WriteFile(path, []byte(`openapi: 3.1.0
paths:
  /status:
    get:
      summary: 状態
  /tenants/{tenantId}/crl:refresh:
    parameters:
      - name: tenantId
    post:
      summary: 取り直し
components:
  parameters:
    get:
      summary: 口ではない
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := openAPIRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET /status", "POST /tenants/{tenantId}/crl:refresh"}; !slices.Equal(got, want) {
		t.Errorf("routes = %q, want %q", got, want)
	}
}

var (
	// openAPIPath は paths の下の口（字下げ 2）。パスに : を含むことがある（crl:refresh）。
	openAPIPath = regexp.MustCompile(`^  (/\S*):\s*$`)
	// openAPIMethod は口の下のメソッド（字下げ 4）。
	openAPIMethod = regexp.MustCompile(`^    (get|put|post|delete|patch|head|options):`)
)

// openAPIRoutes は OpenAPI（YAML）の paths から「メソッド パス」を取り出し、辞書順で返す。
// 外部依存を増やさないため（設計概要 §4）、YAML の部品は使わず、paths の字下げで読む（下流の OpenAPI の書き方に合わせる）。
func openAPIRoutes(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	inPaths, cur := false, ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
			continue
		case !inPaths:
			continue
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			inPaths = false // paths の終わり（次の最上位の項目）
			continue
		}
		if m := openAPIPath.FindStringSubmatch(line); m != nil {
			cur = m[1]
		} else if m := openAPIMethod.FindStringSubmatch(line); m != nil && cur != "" {
			out = append(out, strings.ToUpper(m[1])+" "+cur)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}
