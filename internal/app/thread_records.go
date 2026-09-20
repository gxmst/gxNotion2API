package app

import (
	"context"
	"encoding/json"
)

// The captured browser still uses SpaceInitial successfully, while some
// sessions return only fanout metadata. Retry missing records once through the
// ordinary endpoint. Auth, rate-limit and transport failures are not retried.
func (c *NotionAIClient) syncThreadRecords(ctx context.Context, threadID, table string, ids []string) (map[string]any, error) {
	requests := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		requests = append(requests, map[string]any{"pointer": map[string]any{"table": table, "id": id, "spaceId": c.Session.SpaceID}, "version": -1})
	}
	payload := map[string]any{"requests": requests}
	combined := map[string]any{"recordMap": map[string]any{}}
	for _, endpoint := range []string{"syncRecordValuesSpaceInitial", "syncRecordValues"} {
		body, err := c.postJSONWithReferer(ctx, c.Config.NotionUpstream().API(endpoint), payload, "application/json", c.chatReferer(threadID))
		if err != nil {
			return nil, err
		}
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, err
		}
		for name, raw := range mapValue(response["recordMap"]) {
			incoming := mapValue(raw)
			if incoming == nil {
				continue
			}
			target := mapValue(mapValue(combined["recordMap"])[name])
			if target == nil {
				target = map[string]any{}
				mapValue(combined["recordMap"])[name] = target
			}
			for id, record := range incoming {
				target[id] = record
			}
		}
		missing := make([]map[string]any, 0)
		for _, request := range requests {
			id := stringValue(mapValue(request["pointer"])["id"])
			value := unwrapRecordValue(mapValue(mapValue(combined["recordMap"])[table])[id])
			present := len(value) > 0
			if table == "thread_message" {
				present = len(mapValue(value["step"])) > 0
			}
			if table == "thread" {
				_, hasMessages := value["messages"]
				present = hasMessages || stringValue(value["space_id"]) != ""
			}
			if !present {
				missing = append(missing, request)
			}
		}
		if len(missing) == 0 {
			break
		}
		payload["requests"] = missing
	}
	return combined, nil
}
