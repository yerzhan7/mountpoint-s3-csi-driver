package main

import (
	"strings"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

const gib = 1024 * 1024 * 1024
const mib = 1024 * 1024

func TestMemoryTargetMiB(t *testing.T) {
	testCases := []struct {
		name              string
		memoryBudgetBytes int64
		maxVolumesPerNode int64
		want              int64
		wantUndersized    bool
	}{
		{
			name:              "2Gi across 4 volumes",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: 4,
			want:              512,
		},
		{
			name:              "40Gi across 10 volumes",
			memoryBudgetBytes: 40 * gib,
			maxVolumesPerNode: 10,
			want:              4096,
		},
		{
			name:              "single volume gets the whole limit",
			memoryBudgetBytes: 4 * gib,
			maxVolumesPerNode: 1,
			want:              4096,
		},
		{
			name:              "share is floored, never rounded up",
			memoryBudgetBytes: 2000 * mib,
			maxVolumesPerNode: 3,
			want:              666, // 2000Mi / 3 = 666.66..MiB
		},
		{
			name:              "no memory budget means no target",
			maxVolumesPerNode: 4,
			want:              0,
		},
		{
			name:              "no volume limit means no target",
			memoryBudgetBytes: 2 * gib,
			want:              0,
		},
		{
			name:              "negative volume limit means no target",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: -1,
			want:              0,
		},
		{
			name:              "negative memory budget means no target",
			memoryBudgetBytes: -2 * gib,
			maxVolumesPerNode: 4,
			want:              0,
		},
		{
			name:              "share below Mountpoint's minimum is undersized",
			memoryBudgetBytes: 2 * gib,
			maxVolumesPerNode: 10, // 2Gi / 10 = 204 MiB
			want:              204,
			wantUndersized:    true,
		},
		{
			name:              "share just below Mountpoint's minimum is undersized",
			memoryBudgetBytes: 5111 * mib,
			maxVolumesPerNode: 10,
			want:              511,
			wantUndersized:    true,
		},
		{
			name:              "sub-MiB share is undersized",
			memoryBudgetBytes: 1 * mib,
			maxVolumesPerNode: 4,
			want:              0,
			wantUndersized:    true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, undersized := memoryTargetMiB(tc.memoryBudgetBytes, tc.maxVolumesPerNode)
			assert.Equals(t, tc.want, got)
			assert.Equals(t, tc.wantUndersized, undersized)
		})
	}
}

func TestResolveMemoryTargetMiB(t *testing.T) {
	t.Run("healthy sizing gives the share and no error", func(t *testing.T) {
		target, err := resolveMemoryTargetMiB(2*gib, "limits.memory", 4)
		assert.NoError(t, err)
		assert.Equals(t, int64(512), target)
	})

	t.Run("nothing to divide gives no target and no error", func(t *testing.T) {
		target, err := resolveMemoryTargetMiB(0, "", 4)
		assert.NoError(t, err)
		assert.Equals(t, int64(0), target)
	})

	t.Run("undersized share is an error naming the sizing and the remedy", func(t *testing.T) {
		target, err := resolveMemoryTargetMiB(2*gib, "limits.memory", 10)
		assert.Equals(t, int64(0), target)
		if err == nil {
			t.Fatal("expected an error for an undersized share")
		}
		// Asserted on content because this text is the workload pod's only signal, and nothing in the
		// user's config literally says "204".
		for _, want := range []string{"204 MiB", "maxVolumesPerNode=10", "512 MiB", "5120Mi", "resources.limits.memory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("undersized error names a requests-only budget as the knob to turn", func(t *testing.T) {
		_, err := resolveMemoryTargetMiB(2*gib, "requests.memory", 10)
		if err == nil {
			t.Fatal("expected an error for an undersized share")
		}
		if !strings.Contains(err.Error(), "resources.requests.memory") {
			t.Errorf("error %q does not mention %q", err, "resources.requests.memory")
		}
	})
}

func TestPodMemoryBudgetBytes(t *testing.T) {
	testCases := []struct {
		name       string
		limitEnv   string
		requestEnv string
		wantBytes  int64
		wantField  string
	}{
		{
			name:      "the limit is the budget when set",
			limitEnv:  "2147483648",
			wantBytes: 2 * gib,
			wantField: "limits.memory",
		},
		{
			name:       "the request is the budget when there is no limit",
			requestEnv: "1073741824",
			wantBytes:  1 * gib,
			wantField:  "requests.memory",
		},
		{
			name:       "the limit wins over the request",
			limitEnv:   "2147483648",
			requestEnv: "1073741824",
			wantBytes:  2 * gib,
			wantField:  "limits.memory",
		},
		{
			name:      "neither set means no budget to divide",
			wantBytes: 0,
			wantField: "",
		},
		{
			// Ignored rather than fatal: running with no target is recoverable, refusing every mount on
			// the node is not.
			name:      "a Kubernetes quantity is not a byte count and is ignored",
			limitEnv:  "2Gi",
			wantBytes: 0,
			wantField: "",
		},
		{
			name:       "an unparseable limit falls back to the request",
			limitEnv:   "not-a-number",
			requestEnv: "1073741824",
			wantBytes:  1 * gib,
			wantField:  "requests.memory",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(memoryLimitEnvName, tc.limitEnv)
			t.Setenv(memoryRequestEnvName, tc.requestEnv)
			gotBytes, gotField := podMemoryBudgetBytes()
			assert.Equals(t, tc.wantBytes, gotBytes)
			assert.Equals(t, tc.wantField, gotField)
		})
	}
}
