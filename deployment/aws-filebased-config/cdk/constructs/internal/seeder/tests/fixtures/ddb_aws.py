"""Stateful AWS CLI fixture: evaluate conditional puts, not canned success."""
import json
import os
from pathlib import Path
import sys

_, operation, *args = sys.argv[1:]
request_arg = args[args.index("--cli-input-json") + 1]
request = json.loads(Path(request_arg[7:]).read_text())
state = Path(os.environ["DDB_STATE"])
calls = Path(os.environ["DDB_CALLS"])
history = json.loads(calls.read_text()) if calls.exists() else []
history.append({"operation": operation, "request": request})
calls.write_text(json.dumps(history))
scenario = os.environ.get("DDB_SCENARIO", "")
item = json.loads(state.read_text()) if state.exists() else None


def fail(code):
    service_operation = {"get-item": "GetItem", "put-item": "PutItem"}[operation]
    sys.stderr.write("An error occurred (%s) when calling the %s operation: fixture\n" % (code, service_operation))
    sys.exit(254)


assert request["TableName"] == "test-config"
if operation == "get-item":
    assert request["ConsistentRead"] is True
    assert request["Key"] == {"PK": {"S": "config#test-bridge"}, "SK": {"S": "current"}}
    if scenario == "read-error":
        fail("AccessDeniedException")
    if scenario == "invalid-response":
        print("not json")
    else:
        # AWS CLI's JSON formatter emits nothing for a successful empty result.
        print(json.dumps({"Item": item}) if item is not None else "", end="")
elif operation == "put-item":
    if scenario == "write-error":
        fail("AccessDeniedException")
    if scenario == "write-throttle":
        fail("ProvisionedThroughputExceededException")
    puts = sum(c["operation"] == "put-item" for c in history)
    if scenario in ("conflict-once", "conflict-always", "seed-race") and (puts == 1 or scenario == "conflict-always"):
        if item is None:
            item = request["Item"]
        doc = json.loads(item["data"]["S"])
        doc["version"] = int(item.get("version", {"N": "0"})["N"]) + 1
        item["version"] = {"N": str(doc["version"])}
        item["data"] = {"S": json.dumps(doc)}
        state.write_text(json.dumps(item))
        fail("ConditionalCheckFailedException")
    condition = request["ConditionExpression"]
    if condition == "attribute_not_exists(PK)":
        matched = item is None
    else:
        assert condition == "#v = :expected OR (attribute_not_exists(#v) AND :expected = :zero)"
        assert request["ExpressionAttributeNames"] == {"#v": "version"}
        values = request["ExpressionAttributeValues"]
        assert values[":zero"] == {"N": "0"}
        matched = int((item or {}).get("version", {"N": "0"})["N"]) == int(values[":expected"]["N"])
    if not matched:
        fail("ConditionalCheckFailedException")
    written = request["Item"]
    assert json.loads(written["data"]["S"])["version"] == int(written["version"]["N"])
    state.write_text(json.dumps(written))
    print("{}")
else:
    raise AssertionError("unsupported DynamoDB operation: " + operation)
