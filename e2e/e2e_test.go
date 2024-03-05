package e2e

import (
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/kluster-manager/cluster-gateway/e2e/framework"
	// per-package e2e suite
	_ "github.com/kluster-manager/cluster-gateway/e2e/roundtrip"
)

func TestMain(m *testing.M) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	framework.ParseFlags()
	os.Exit(m.Run())
}

func TestE2E(t *testing.T) {
	RunE2ETests(t)
}
