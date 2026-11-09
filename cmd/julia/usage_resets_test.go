package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"julia/internal/core"
	"julia/internal/provider"
)

func TestUsageResetSettingReachesCodexAndSeparateReviewer(t *testing.T) {
	for _, name := range []string{"codex", "openai"} {
		for _, enabled := range []bool{false, true} {
			label := "disabled"
			if enabled {
				label = "enabled"
			}
			t.Run(name+"/"+label, func(t *testing.T) {
				binary, capture := reviewerProtocolFixture(t)
				cfg := reviewerFactoryConfig(t, name, binary)
				cfg.UseUsageResets = enabled
				backend, review, err := newBackend(context.Background(), cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				if codex, ok := backend.(*provider.Codex); ok && codex.UseUsageResets != enabled {
					t.Fatal("executing Codex lost usage reset setting")
				}
				if _, err := review(context.Background(), core.Approval{Task: "Inspect fixture"}); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				queried := strings.Contains(string(data), `"method":"account/rateLimits/read"`)
				if queried != enabled {
					t.Fatalf("reviewer queried reset eligibility = %v, enabled = %v", queried, enabled)
				}
			})
		}
	}
}
