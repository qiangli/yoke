// Package builtin is the single place where account adapters that live in
// subpackages join the default registry. The in-package LLM adapter
// self-registers from pkg/accounts; every subpackage adapter (cloudbox now,
// digitalocean and aws next) registers here, one line each, so `account
// discover` runs every registered Discover-capable adapter without the
// registry itself importing providers and without an import cycle: builtin
// imports the adapter packages, and hosts import builtin.
//
// Hosts embedding NewAccountCmd link this package for its side effect:
//
//	_ "github.com/qiangli/yoke/pkg/accounts/builtin"
package builtin

import (
	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/adapters/cloudbox"
)

func init() {
	// One line per subpackage adapter, merged in story acceptance order.
	accounts.MustRegisterAdapter(cloudbox.New())
}
