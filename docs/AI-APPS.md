# AI features in generated apps

The model that builds an app and the model the finished app calls are separate.
Build routing does not automatically give an application GPT credentials.

## Text generation

Add `"ai":"ollama"` to a version-1 runtime manifest. At build, service startup,
and check time, harness++ supplies:

| Variable | Value |
| --- | --- |
| `OLLAMA_BASE_URL` | `http://127.0.0.1:11434` |
| `OLLAMA_MODEL` | `gemma4:cloud` |

The existing Ollama daemon owns the cloud sign-in. No Ollama password or API key
is copied into the app. The daemon must be running and the account must have model
access and available quota. Declaring the integration does not download a model
or create a paid subscription.

The app backend sends a request to `${OLLAMA_BASE_URL}/api/chat`:

```json
{
  "model": "<value of OLLAMA_MODEL>",
  "messages": [{"role": "user", "content": "Write a short story about a lost kite."}],
  "stream": false
}
```

Read `message.content` from a successful JSON response. Check both the HTTP status
and Ollama's `error` field. Use a bounded request, validate input length, and return
a useful error for unavailable models, exhausted quota, or an offline daemon.
Cancel upstream work when the user cancels. Do not send browser requests directly
to Ollama or expose its administrative endpoints through the app.

Use stubs in automated startup checks. Readiness should check whether the app can
serve requests, not spend cloud quota generating a sample. Live inference should
be tested separately and clearly identified. The harness build-usage panel does
not account for calls made by the running app.

This is configuration for a trusted local app, not a per-app quota or security
boundary. Runtime network access is general; undeclared code can also contact
loopback services. Do not expose the app publicly with your signed-in daemon behind it.

## Stories with illustrations

Gemma can write the story. This integration does **not** provide image generation.
A complete illustrated-story app needs a separately selected image model/API,
credentials, image storage, and error handling. None is configured by default.

A useful request today:

> Build a story-writing app. Let me choose a topic and reading age. Generate the
> story through the backend using the Ollama runtime. Save stories so I can return
> to them. Show progress and let me cancel. Let me upload illustrations; do not
> claim images are AI-generated. Test the UI, saving, validation, and provider
> failure using a stub without making paid calls.

For AI-generated illustrations, configure and verify an image provider first.
An image URL, placeholder, or SVG drawn by the app is not evidence of image-model
inference.
