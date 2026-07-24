import { Type } from "@sinclair/typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "get_time",
    label: "Get Time",
    description: "Returns the current UTC time as an ISO-8601 string.",
    parameters: Type.Object({}),
    execute: async () => {
      const time = new Date().toISOString();
      return {
        content: [{ type: "text", text: `Current UTC time: ${time}` }],
        details: { time },
      };
    },
  });
}
