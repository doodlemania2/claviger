lint: shellcheck
	golangci-lint run -c ./golangci.yml ./...

# CI runs the same check with the pinned koalaman/shellcheck-alpine image.
shellcheck:
	shellcheck entrypoint.sh scripts/*.sh

test-entrypoint:
	TEST_SH=sh sh scripts/test_entrypoint_encryption.sh

test:
	go test ./... -v --cover

jstypes:
	go run ./plugins/jsvm/internal/types/types.go

test-report:
	go test ./... -v --cover -coverprofile=coverage.out
	go tool cover -html=coverage.out

# Starts a Claviger instance prepared for the OpenID Foundation conformance suite and
# prints the suite's test plan config. See docs/oidc-conformance.md.
oidc-conformance:
	./scripts/oidc-conformance-instance.sh

third-party-notices:
	./scripts/generate-third-party-notices.sh
