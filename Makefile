.PHONY: all
all: build

.PHONY: build
build: combadge bmus

.PHONY: combadge
combadge:
	@cd combadge && npm ci && npm run build

# The console frontend's build output, internal/console/assets, is
# committed: build doesn't need this. Run it after changing web/.
.PHONY: web
web:
	@cd web && npm ci && npm run build

.PHONY: bmus
bmus:
	go build -o bmus ./cmd/bmus
