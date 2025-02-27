# nostr-webhook

A flexible event broker for Nostr that connects to relays, listens for events, and triggers webhooks when matching events are received. It can also run scheduled tasks and publish events to Nostr relays.

![](https://raw.githubusercontent.com/mattn/nostr-webhook/main/static/description.png)

## Features

- **Event Filtering**: Listen for specific Nostr events based on kind, content patterns, and mentions
- **Webhook Triggers**: Send matching events to configured HTTP endpoints
- **Scheduled Tasks**: Run cron jobs that call endpoints and publish responses to Nostr relays
- **Relay Management**: Configure and manage feed and post relays
- **Web Interface**: Manage hooks, watches, tasks, and proxies through a web UI
- **Docker Support**: Easy deployment with Go or Docker Compose
- **Authentication**: Support for NIP-42 relay authentication
- **Cloudflare Access**: Optional support for Cloudflare Access for admin endpoints

## Installation

### Using Go

```bash
go install github.com/mattn/nostr-webhook@latest
```

### Using Docker

```bash
# Clone the repository
git clone https://github.com/mattn/nostr-webhook.git
cd nostr-webhook

# Configure your environment
cp .env.example .env
# Edit .env with your Nostr private key
# NOSTR_PRIVATE_KEY=nsec1...

# Start the containers
docker-compose up -d
```

## Configuration

Configuration is done through a `config.json` file:

```json
{
  "auth": {
    "requireCloudflare": false
  },
  "relays": {
    "feed": [
      {"relay": "wss://relay.example.com", "enabled": true},
      {"relay": "wss://another-relay.example.com", "enabled": false}
    ],
    "post": [
      {"relay": "wss://relay.example.com", "enabled": true},
      {"relay": "wss://another-relay.example.com", "enabled": false}
    ]
  }
}
```

- **auth.requireCloudflare**: Whether to require Cloudflare Access authentication
- **relays.feed**: List of relays to subscribe to for incoming events
- **relays.post**: List of relays to publish events to

## Environment Variables

- `NOSTR_PRIVATE_KEY`: Your Nostr private key in nsec format used for NIP-42 relay authentication

## Usage

### Web Interface

Access the web interface at http://localhost:8989/ to manage:

- **Hooks**: Configure event filters and webhook endpoints
- **Watches**: Monitor for specific events
- **Tasks**: Set up scheduled jobs
- **Proxies**: Configure proxy settings

### API Endpoints

- `GET /info`: Get relay and version information
- `GET /hooks`: List all hooks
- `POST /hooks`: Create a new hook
- `GET /watches`: List all watches
- `POST /watches`: Create a new watch
- `GET /tasks`: List all tasks
- `POST /tasks`: Create a new task
- `GET /reload`: Reload relay configurations

### Webhook Response Handling

When a webhook endpoint is called, nostr-webhook sends the Nostr event as JSON in the request body. The webhook can respond in two ways:

1. **Return a valid Nostr event**: If the response is a valid signed Nostr event (or array of events) in JSON format, nostr-webhook will publish these events to the configured post relays.

2. **Return any other response**: If the response is not a valid Nostr event, nostr-webhook will log the response content but will not attempt to publish anything to relays.

This behavior allows webhooks to either:

- Receive an event and respond with an event.
- Simply receive an event and trigger some other process or workflow.

Example use:

- Read notes from a private relay (e.g. [SW2](https://github.com/bitvora/sw2))
- When filters are met, trigger an n8n workflow (e.g. [Nostr-n8n](https://github.com/r0d8lsh0p/nostr-n8n))
- Optionally publish notes back to the same relay, or others

## Docker Deployment

The included Docker Compose configuration sets up:

1. The nostr-webhook application
2. A PostgreSQL database for storing configuration

```bash
# Start the services
docker-compose up -d

# View logs
docker-compose logs -f

# Stop the services
docker-compose down
```

## Author

Yasuhiro Matsumoto (a.k.a. mattn)

## Contributors

[Rod Bishop](https://njump.me/npub1r0d8u8mnj6769500nypnm28a9hpk9qg8jr0ehe30tygr3wuhcnvs4rfsft)

## Future Enhancements

- Use Nostr private key to sign events for publishing (currently only used for NIP-42 authorization)
- Improve admin user interface layout
- Improve documentation on API methods
- Provide example uses of webhook triggers, e.g. sample n8n workflows

## License

MIT
