# Video Recording

Capture browser automation sessions as video for debugging, documentation, or verification. Produces WebM (VP8/VP9 codec).

## Basic Recording

```bash
# Start recording (the filename goes here)
playwright-cli video-start demo.webm

# Perform actions
playwright-cli open https://example.com
playwright-cli snapshot
playwright-cli click e1
playwright-cli fill e2 "test input"

# Stop and save
playwright-cli video-stop
```

## Best Practices

### 1. Use Descriptive Filenames

```bash
# Include context in filename (passed to video-start)
playwright-cli video-start recordings/login-flow-2024-01-15.webm
playwright-cli video-start recordings/checkout-test-run-42.webm
```

## Tracing vs Video

| Feature | Video | Tracing |
|---------|-------|---------|
| Output | WebM file | Trace file (viewable in Trace Viewer) |
| Shows | Visual recording | DOM snapshots, network, console, actions |
| Use case | Demos, documentation | Debugging, analysis |
| Size | Larger | Smaller |

## Limitations

- Recording adds slight overhead to automation
- Large recordings can consume significant disk space
