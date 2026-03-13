# Package Level Requirements

Tests for the sdk-integration-testing project should verify:

- **Mocking**: Providing mock implementations of NATS, Registry, and Gateway for plugin isolation tests.
- **Contract Testing**: Verification that plugins adhere to the `runner.Plugin` interface.
