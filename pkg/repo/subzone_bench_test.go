package repo_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
)

const benchZone = "example.com."

var benchAuthor = object.Identity{Name: "bench", Email: "bench@example.com"}

type rrsetFixture struct {
	fqdn   string
	rrtype string
	rrs    []dns.RR
}

func BenchmarkAddSubzone(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	cases := []struct {
		name     string
		subzone  string
		hosts    int
		withNest bool
	}{
		{name: "Empty", subzone: "sub", hosts: 0},
		{name: "Small100", subzone: "sub", hosts: 100},
		{name: "Large10000", subzone: "sub", hosts: 10_000},
		{name: "DeepNestedName", subzone: "a.b.c.d", hosts: 100},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			fixtures := buildSubzoneFixtures(b, benchZone, tc.subzone, tc.hosts, tc.withNest)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				r := mustOpenBenchRepo(b)
				b.StartTimer()

				stageFixtures(b, ctx, r, fixtures)
				commitFixtures(b, ctx, r, "bench add subzone")

				b.StopTimer()
				if err := r.Close(); err != nil {
					b.Fatalf("close repo: %v", err)
				}
			}
		})
	}
}

func BenchmarkDeleteSubzone(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	cases := []struct {
		name     string
		subzone  string
		hosts    int
		withNest bool
	}{
		{name: "EmptyLeaf", subzone: "sub", hosts: 0},
		{name: "Small100Leaf", subzone: "sub", hosts: 100},
		{name: "Large10000Leaf", subzone: "sub", hosts: 10_000},
		{name: "Small100Cascade", subzone: "sub", hosts: 100, withNest: true},
		{name: "Large10000Cascade", subzone: "sub", hosts: 10_000, withNest: true},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			fixtures := buildSubzoneFixtures(b, benchZone, tc.subzone, tc.hosts, tc.withNest)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				r := mustOpenBenchRepo(b)
				stageFixtures(b, ctx, r, fixtures)
				commitFixtures(b, ctx, r, "bench seed subzone")
				b.StartTimer()

				stageFixtureDeletes(r, benchZone, fixtures)
				commitFixtures(b, ctx, r, "bench delete subzone")

				b.StopTimer()
				if err := r.Close(); err != nil {
					b.Fatalf("close repo: %v", err)
				}
			}
		})
	}
}

func BenchmarkSubzoneChurn(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()

	b.Run("Small100", func(b *testing.B) {
		r := mustOpenBenchRepo(b)
		defer func() {
			if err := r.Close(); err != nil {
				b.Fatalf("close repo: %v", err)
			}
		}()

		fixtures := buildSubzoneFixtures(b, benchZone, "churn", 100, true)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stageFixtures(b, ctx, r, fixtures)
			commitFixtures(b, ctx, r, "bench churn add")
			stageFixtureDeletes(r, benchZone, fixtures)
			commitFixtures(b, ctx, r, "bench churn delete")
		}
	})
}

func mustOpenBenchRepo(b *testing.B) *repo.Repo {
	b.Helper()
	r, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		b.Fatalf("open repo: %v", err)
	}
	if err := r.Init(context.Background(), benchZone); err != nil {
		_ = r.Close()
		b.Fatalf("init repo: %v", err)
	}
	return r
}

func buildSubzoneFixtures(b *testing.B, zoneName, subzone string, hostCount int, withNestedChildren bool) []rrsetFixture {
	b.Helper()
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic benchmark data
	zoneNoDot := strings.TrimSuffix(zoneName, ".")
	subzoneFQDN := dns.Fqdn(fmt.Sprintf("%s.%s", subzone, zoneNoDot))

	fixtures := make([]rrsetFixture, 0, hostCount+2)
	nsTarget := dns.Fqdn("ns1." + subzoneFQDN)
	fixtures = append(fixtures, rrsetFixture{
		fqdn:   subzoneFQDN,
		rrtype: "NS",
		rrs: []dns.RR{
			mustBenchRR(b, fmt.Sprintf("%s 300 IN NS %s", subzoneFQDN, nsTarget)),
		},
	})
	fixtures = append(fixtures, rrsetFixture{
		fqdn:   nsTarget,
		rrtype: "A",
		rrs: []dns.RR{
			mustBenchRR(b, fmt.Sprintf("%s 300 IN A 192.0.2.1", nsTarget)),
		},
	})

	for i := 0; i < hostCount; i++ {
		label := fmt.Sprintf("h%05d", i)
		if withNestedChildren && i%2 == 0 {
			label = fmt.Sprintf("%s.child", label)
		}
		owner := dns.Fqdn(fmt.Sprintf("%s.%s", label, subzoneFQDN))
		ip := fmt.Sprintf("198.%d.%d.%d", rng.Intn(255), rng.Intn(255), (i%250)+1)
		fixtures = append(fixtures, rrsetFixture{
			fqdn:   owner,
			rrtype: "A",
			rrs: []dns.RR{
				mustBenchRR(b, fmt.Sprintf("%s 300 IN A %s", owner, ip)),
			},
		})
	}
	return fixtures
}

func stageFixtures(b *testing.B, ctx context.Context, r *repo.Repo, fixtures []rrsetFixture) {
	b.Helper()
	for _, f := range fixtures {
		if err := r.Set(ctx, f.rrs); err != nil {
			b.Fatalf("set %s %s: %v", f.fqdn, f.rrtype, err)
		}
	}
}

func stageFixtureDeletes(r *repo.Repo, zoneName string, fixtures []rrsetFixture) {
	for _, f := range fixtures {
		r.Delete(relativeNameFromZone(f.fqdn, zoneName), f.rrtype)
	}
}

func commitFixtures(b *testing.B, ctx context.Context, r *repo.Repo, msg string) {
	b.Helper()
	if _, err := r.Commit(ctx, benchAuthor, msg); err != nil {
		b.Fatalf("commit %q: %v", msg, err)
	}
}

func mustBenchRR(b *testing.B, text string) dns.RR {
	b.Helper()
	rr, err := dns.NewRR(text)
	if err != nil {
		b.Fatalf("dns.NewRR(%q): %v", text, err)
	}
	return rr
}

func relativeNameFromZone(fqdn, zoneName string) string {
	fqdn = strings.ToLower(dns.Fqdn(fqdn))
	zoneName = strings.ToLower(dns.Fqdn(zoneName))
	switch {
	case fqdn == zoneName:
		return "@"
	case strings.HasSuffix(fqdn, "."+zoneName):
		return strings.TrimSuffix(fqdn, "."+zoneName)
	case strings.HasSuffix(fqdn, zoneName):
		out := strings.TrimSuffix(fqdn, zoneName)
		return strings.TrimSuffix(out, ".")
	default:
		return strings.TrimSuffix(fqdn, ".")
	}
}
