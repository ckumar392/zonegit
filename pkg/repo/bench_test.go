package repo_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/miekg/dns"
)

func buildZone(b *testing.B, n int) (*repo.Repo, []string) {
	b.Helper()
	r, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		b.Fatal(err)
	}
	if err := r.Init(context.Background(), "foo.com."); err != nil {
		b.Fatal(err)
	}
	names := make([]string, n)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("h%07d", i)
		names[i] = name
		fqdn := name + ".foo.com."
		rr, err := dns.NewRR(fmt.Sprintf("%s 300 IN A 10.%d.%d.%d",
			fqdn, (i>>16)&0xff, (i>>8)&0xff, i&0xff))
		if err != nil {
			b.Fatal(err)
		}
		if err := r.Set(ctx, []dns.RR{rr}); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := r.Commit(ctx, object.Identity{Name: "bench", Email: "b@b"}, "seed"); err != nil {
		b.Fatal(err)
	}
	return r, names
}

func BenchmarkLookup_10k(b *testing.B) {
	r, names := buildZone(b, 10_000)
	defer r.Close()
	ctx := context.Background()
	_, head, err := r.Head(ctx)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		name := names[rng.Intn(len(names))]
		rs, err := r.Lookup(ctx, head, name, "A")
		if err != nil {
			b.Fatal(err)
		}
		if len(rs.RRs) == 0 {
			b.Fatal("empty rrset")
		}
	}
}

func BenchmarkCommitOneChange_10k(b *testing.B) {
	r, names := buildZone(b, 10_000)
	defer r.Close()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(2))
	ident := object.Identity{Name: "bench", Email: "b@b"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		name := names[rng.Intn(len(names))]
		fqdn := name + ".foo.com."
		rr, err := dns.NewRR(fmt.Sprintf("%s 300 IN A 192.0.2.%d", fqdn, i&0xff))
		if err != nil {
			b.Fatal(err)
		}
		if err := r.Set(ctx, []dns.RR{rr}); err != nil {
			b.Fatal(err)
		}
		if _, err := r.Commit(ctx, ident, "tick"); err != nil {
			b.Fatal(err)
		}
	}
}
