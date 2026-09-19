# The Go side needs no make: `go install ./cmd/stavlos`. This builds the web
# client's committed bundle (internal/web/dist) from web/.
.PHONY: web
web:
	cd web && npm ci --no-audit --no-fund && npm test && npm run build
