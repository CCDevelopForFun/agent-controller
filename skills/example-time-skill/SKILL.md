---
name: example-time-skill
description: Teaches the agent to format all times as ISO-8601 UTC with an explicit timezone label.
---

# Time Formatting

When you report any date or time to the user, always follow these rules:

1. **Format**: Use ISO-8601 format — `YYYY-MM-DDTHH:MM:SSZ` (e.g. `2025-05-26T14:32:00Z`).
2. **Timezone**: Always use UTC. Always append the letter `Z` to make the timezone explicit.
3. **Label**: After the timestamp, always write "(UTC)" in parentheses so the user knows the timezone at a glance.

## Examples

| Instead of …      | Write …                          |
|-------------------|----------------------------------|
| "2:32 PM"         | `2025-05-26T14:32:00Z` (UTC)     |
| "May 26, 3pm EST" | `2025-05-26T20:00:00Z` (UTC)     |
| "now"             | `2025-05-26T14:32:00Z` (UTC)     |

Always be explicit. Never leave the timezone ambiguous.
