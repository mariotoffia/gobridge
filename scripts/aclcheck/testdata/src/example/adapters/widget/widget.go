package widget

// Use consumes the ACL wrapper and stays SDK-free: this non-ACL file
// never names a vendor SDK type, so it raises no findings.
func Use(g Good) string { return g.Body() }
