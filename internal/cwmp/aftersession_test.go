package cwmp_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/cwmp"
)

func TestRunSessionRunsAfterSessionWhenTheSessionEnds(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		scripts  []string
		statuses []int
		wantErr  bool
	}{
		"clean end":      {[]string{informResponseEnvelope, ""}, []int{200, http.StatusNoContent}, false},
		"failed session": {[]string{""}, []int{http.StatusInternalServerError}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tr, s, _, _ := buildRunSessionScaffold(t, tc.scripts, tc.statuses, nil)
			after := &cwmp.AfterSession{}
			ran := 0
			after.Add(func() { ran++ })

			err := cwmp.RunSession(context.Background(), cwmp.RunSessionOptions{
				Tracker:       tr,
				Tree:          buildTree(t),
				Session:       s,
				Clock:         func() time.Time { return fixedTime },
				DeviceIDPaths: testDeviceIDPaths,
				AfterSession:  after,
			}, cwmp.TriggerStartup)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunSession error = %v, want error %v", err, tc.wantErr)
			}
			if ran != 1 {
				t.Fatalf("queued work ran %d times, want 1", ran)
			}

			after.Run()
			if ran != 1 {
				t.Errorf("the queue was not cleared: work ran %d times", ran)
			}
		})
	}
}
