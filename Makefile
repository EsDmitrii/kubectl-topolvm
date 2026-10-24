BINARY := kubectl-topolvm

.PHONY: build test lint golden

build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) ./cmd/kubectl-topolvm

test:
	go test -race ./...

lint:
	golangci-lint run

golden:
	go test ./internal/output/... -run TestGolden -update
	go test ./internal/rbac/... -run TestRBACDefaultGolden -update
	-git diff --stat -- testdata docs/rbac.yaml
