.PHONY: test test-integration lint js

test:
	go test ./...

# The JS buyer tests skip without node_modules, so `make js` first for full coverage.
test-integration:
	go test -tags=integration -p 1 ./...

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

# The buyers in js/ are stock @x402/* clients driven by the integration tests. They
# skip when the SDK is absent, so installing it is opt-in and a Go-only checkout is
# never blocked on npm.
js: js/node_modules

js/node_modules: js/package.json
	cd js && npm install --no-fund --no-audit
	touch $@
