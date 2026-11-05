package admin

import (
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// writeSecretResponseHeaders marks a response as secret-bearing so it is never cached or content-sniffed.
// Split from writeSecretDownloadHeaders because the CPA export endpoints return an inline JSON array the frontend names itself, so they need the cache guards without the download disposition.
func writeSecretResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
}

func writeSecretDownloadHeaders(c *gin.Context, filename string) {
	writeSecretResponseHeaders(c)
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
}

// accountRowToExportEntry turns an account row into its CPA export entry.
func accountRowToExportEntry(row *database.AccountRow, proxies exportProxyResolver) (any, bool) {
	entry, ok := accountRowToCPAExportEntry(row, proxies)
	if !ok {
		return nil, false
	}
	return entry, true
}
