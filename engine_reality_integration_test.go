//go:build with_cronet_test

package cronet

import "testing"

func TestRealityEnginePolicy(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine()
	defer engine.Destroy()
	key := [32]byte{9}
	if err := engine.SetReality(key, [8]byte{}); err != nil {
		t.Fatal(err)
	}
	params := NewEngineParams()
	defer params.Destroy()
	params.SetEnableCheckResult(false)
	params.SetEnableQuic(true)
	if got := engine.StartWithParams(params); got != ResultIllegalArgument {
		t.Fatalf("REALITY+QUIC = %v", got)
	}
	params.SetEnableQuic(false)
	if err := engine.SetStrictECH(true); err != nil {
		t.Fatal(err)
	}
	if got := engine.StartWithParams(params); got != ResultIllegalArgument {
		t.Fatalf("REALITY+StrictECH = %v", got)
	}
	if err := engine.SetStrictECH(false); err != nil {
		t.Fatal(err)
	}
	if got := engine.StartWithParams(params); got != ResultSuccess {
		t.Fatal(got)
	}
	defer engine.Shutdown()
	if err := engine.SetReality(key, [8]byte{1}); err == nil {
		t.Fatal("changed running engine credentials")
	}
}
