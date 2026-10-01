package previewenv_test

import (
	"context"
	"fmt"
	"time"

	previewenv "github.com/feng111627/preview-env-lifecycle-go"
)

func ExampleManager() {
	clk := func() time.Time { return time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC) }
	store := previewenv.NewStore(100)
	m := previewenv.NewManager(store, time.Hour, previewenv.WithClock(clk))
	m.SetQuota("alice", previewenv.Quota{MaxEnvironments: 3, MaxUnits: 20})

	ctx := context.Background()
	env, err := m.Create(ctx, previewenv.CreateRequest{Name: "feat-x", Owner: "alice", Units: 5})
	fmt.Println("create:", label(env.State, err))

	active, err := m.Claim(ctx, env.ID, "alice")
	fmt.Println("claim:", label(active.State, err))

	err = m.Destroy(ctx, env.ID, "bob")
	fmt.Println("destroy by non-owner:", previewenv.ReasonOf(err))

	err = m.Destroy(ctx, env.ID, "alice")
	fmt.Println("destroy by owner:", previewenv.ReasonOf(err))
	// Output:
	// create: reserved
	// claim: active
	// destroy by non-owner: permission_denied
	// destroy by owner:
}

func label(s previewenv.State, err error) string {
	if r := previewenv.ReasonOf(err); r != "" {
		return string(s) + " " + string(r)
	}
	return string(s)
}
