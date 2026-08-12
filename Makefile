.PHONY: test test-integration lint js js-test

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
