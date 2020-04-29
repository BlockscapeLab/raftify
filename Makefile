# Testing
# Testing for raftify needs to happen sequentially. This makes sure
# the tests are not run in parallel.
tests:
	@echo "Running tests for Raftify..."
	@go test -v -cover -coverprofile=coverage.txt -covermode=atomic ./...
	@go tool cover -html=coverage.txt -o coverage.html
	@echo "Tests finished"