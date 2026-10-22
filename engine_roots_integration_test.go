//go:build with_cronet_test

package cronet

import "testing"

func TestEngineEmptyTrustedRoots(t *testing.T) {
	if err := checkLibrary(); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine()
	defer engine.Destroy()
	if !engine.SetTrustedRootCertificates("") {
		t.Fatal("set empty trusted root certificates")
	}
	params := NewEngineParams()
	defer params.Destroy()
	params.SetEnableCheckResult(false)
	params.SetEnableQuic(false)
	if got := engine.StartWithParams(params); got != ResultSuccess {
		t.Fatalf("start engine = %v", got)
	}
	defer engine.Shutdown()
}
