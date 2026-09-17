package watch_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac-metering/internal/watch"
)

var _ = Describe("networking Watch registration", func() {
	It("registers only billable networking payloads", func() {
		filter := watch.BuildFilter(false, false, false)

		Expect(filter).To(Equal("has(event.external_ip) || has(event.nat_gateway)"))
		Expect(filter).NotTo(ContainSubstring("virtual_network"))
		Expect(filter).NotTo(ContainSubstring("subnet"))
		Expect(filter).NotTo(ContainSubstring("security_group"))
	})
})
