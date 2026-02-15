# go-ai-chatbot

## Local setup

1. Copy `.env.example` to `.env`.
2. Fill in `OPENAI_API_KEY`, `SUPABASE_URL`, and `SUPABASE_SERVICE_ROLE`.
3. Start the server:

```bash
go run .
```

The app loads local `.env` values automatically for development.

## Notes on secrets

- Never commit `.env` files.
- `/v1/config` only reports non-sensitive config state flags.
