# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.1] - 2025-05-22

### Added
- Enhanced debug logging:
    - Log the full `ClientOptions` structure upon client initialization when `Debug: true`.
    - Log the exact payload string being signed for WebSocket handshake and operations (subscribe/publish) when `Debug: true`.

### Changed
- No breaking changes.

### Fixed
- No specific bug fixes in this version, focus was on improving debuggability.
