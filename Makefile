# 开发便捷入口（CI 的权威流程见 .github/workflows/ci.yml，此处仅为本地等价命令）
.PHONY: vet test test-it test-st fmt gen-ts

vet:
	go vet -tags "it st" ./...

test:
	go test ./...

test-it:
	go test -count=1 -tags it ./...

test-st:
	go test -count=1 -tags st ./...

fmt:
	gofmt -w cmd internal

gen-ts:
	./tools/scripts/gen-ts.sh
