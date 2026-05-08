package bridgecfg_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
)

func TestAdminAPIDefaults(t *testing.T) {
	got := bridgecfg.AdminAPIDefaults()
	if got.AdminAddr != bridgecfg.DefaultAdminAddr {
		t.Errorf("AdminAddr = %q, want %q", got.AdminAddr, bridgecfg.DefaultAdminAddr)
	}
	if got.MonitorAddr != "" {
		t.Errorf("MonitorAddr = %q, want empty", got.MonitorAddr)
	}
	if got.AdminAPIKey != "" {
		t.Errorf("AdminAPIKey = %q, want empty (operator must set credential URI)", got.AdminAPIKey)
	}
	if got.CORSOrigins != "" {
		t.Errorf("CORSOrigins = %q, want empty", got.CORSOrigins)
	}
}

func TestDefaultSQSAutoExtend(t *testing.T) {
	v := bridgecfg.DefaultSQSAutoExtend()
	if v == nil {
		t.Fatal("DefaultSQSAutoExtend returned nil")
	}
	if !*v {
		t.Errorf("DefaultSQSAutoExtend() = false, want true")
	}
	// Each call returns a fresh pointer so a caller mutating the
	// dereference does not poison the next caller's default.
	other := bridgecfg.DefaultSQSAutoExtend()
	if v == other {
		t.Errorf("DefaultSQSAutoExtend should return distinct pointers across calls")
	}
}
