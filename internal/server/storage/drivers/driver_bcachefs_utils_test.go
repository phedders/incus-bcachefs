package drivers

import (
	"testing"
)

// TestOptimizedImages checks that image caching defaults to on and can be disabled.
func TestOptimizedImages(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   bool
	}{
		{"Default", nil, true},
		{"Empty", map[string]string{"bcachefs.optimized_images": ""}, true},
		{"Enabled", map[string]string{"bcachefs.optimized_images": "true"}, true},
		{"Disabled", map[string]string{"bcachefs.optimized_images": "false"}, false},
		{"Disabled zero", map[string]string{"bcachefs.optimized_images": "0"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &bcachefs{common: common{config: tt.config}}
			got := d.optimizedImages()
			if got != tt.want {
				t.Fatalf("optimizedImages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSubvolumeHasChildren checks the detection of subvolumes below a subvolume.
func TestSubvolumeHasChildren(t *testing.T) {
	tests := []struct {
		name    string
		subVols []string
		relPath string
		want    bool
	}{
		{"No subvolumes", nil, "", false},
		{"Root with subvolumes", []string{"a"}, "", true},
		{"Leaf", []string{"a", "a/b"}, "a/b", false},
		{"Child", []string{"a", "a/b"}, "a", true},
		{"Nested child", []string{"a", "a/dir/b"}, "a", true},
		{"Sibling prefix", []string{"a", "ab"}, "a", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := subvolumeHasChildren(tt.subVols, tt.relPath)
			if got != tt.want {
				t.Fatalf("subvolumeHasChildren(%v, %q) = %v, want %v", tt.subVols, tt.relPath, got, tt.want)
			}
		})
	}
}

// TestParseBcachefsVersion checks the parsing of `bcachefs version` output.
func TestParseBcachefsVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"Plain", "1.39.6\n", "1.39.6"},
		{"No newline", "1.39.6", "1.39.6"},
		{"Leading v", "v1.39.6\n", "1.39.6"},
		{"Tool name prefix", "bcachefs-tools v1.39.6\n", "1.39.6"},
		{"Pre-release suffix", "1.39.6~20260918124200.gbp772d5cd02021\n", "1.39.6~20260918124200.gbp772d5cd02021"},
		{"Leading blank line", "\n1.39.6\n", "1.39.6"},
		{"Empty", "", ""},
		{"Whitespace only", "   \n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBcachefsVersion(tt.in)
			if got != tt.want {
				t.Fatalf("parseBcachefsVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
