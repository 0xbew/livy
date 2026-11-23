package doctor

import (
	"time"

	"github.com/0xbew/livy/internal/denylist"
	"github.com/0xbew/livy/internal/hook"
)

// checkDenylist is the identity denylist's row (internal/denylist.Check decides it).
func checkDenylist(r *Report) {
	status, detail := denylist.Check(hook.Settings(), hook.DataDir(), time.Now())
	r.add(status, "denylist", "%s", detail)
}
