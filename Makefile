.PHONY: test test-integration test-e2e lint js js-test

test:
	go test ./...

# The JS buyer tests skip without node_modules, so `make js` first for full coverage.
test-integration:
	go test -tags=integration -p 1 ./...

# One payment, all the way through: a real in-memory node, our facilitator, the
# canonical middleware, and the JS buyer. e2e/ is its own module so the root
# ./... never starts a chain, which is also why it needs its own target. The
# buyer is half the assertion, so the package has to be built first.
test-e2e: js/dist
	cd e2e && go test -count=1 ./...

lint:
	go vet ./...
	cd e2e && go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

# The buyers in js/ are stock @x402/* clients driven by the integration tests. They
# skip when the SDK is absent, so installing it is opt-in and a Go-only checkout is
# never blocked on npm. buy.mjs imports the mechanism by package name, the way a
# stranger would, so the package has to be built and not merely installed.
js: js/dist

# The mechanism's own tests, including the one holding its sign doc byte-identical
# to the wallet's. A safety net nothing runs is not a safety net.
js-test: js/dist
	cd js && npm test

js/node_modules: js/package.json
	cd js && npm install --no-fund --no-audit
	touch $@

js/dist: js/node_modules js/tsconfig.json js/tsdown.config.ts $(wildcard js/src/*.ts)
	cd js && npm run build
	touch $@
