#!/usr/bin/env python3
"""Generate the checked-in OpenAPI contract. No credentials or network calls."""
import json
from pathlib import Path

def string(**kw): return {"type": "string", **kw}
def ref(name): return {"$ref": f"#/components/schemas/{name}"}
def obj(props, required=()): return {"type": "object", "additionalProperties": False, "properties": props, "required": list(required)}
stages = ["saved", "applied", "talking", "interview", "offer", "hired", "rejected", "archived"]
number = {"type": ["number", "null"], "minimum": 0}
date = string(description="RFC3339 timestamp, or empty string when unknown.", pattern=r"^$|^\d{4}-\d{2}-\d{2}T")
url = string(maxLength=2048, description="Absolute http(s) URL, or empty string; no URL credentials.")
lines = {"type": "array", "maxItems": 100, "items": string(maxLength=200), "default": []}
fields = {
    "company": string(minLength=1, maxLength=300), "title": string(minLength=1, maxLength=300),
    "url": url, "description": string(maxLength=20000), "stage": string(enum=stages, default="saved"),
    "remote": string(enum=["unknown", "remote", "hybrid", "onsite"], default="unknown"),
    "location": string(maxLength=1000, description="Geographic/work-authorization restrictions; remote does not imply eligible."),
    "employment": string(enum=["", "full-time", "part-time", "contract", "freelance"]),
    "salary_min": number, "salary_max": number, "currency": string(pattern="^$|^[A-Z]{3}$"),
    "salary_period": string(enum=["", "year", "month", "hour"]),
    "fit_score": {**number, "maximum": 10, "description": "Null means not assessed. A number requires fit_summary."},
    "fit_summary": string(maxLength=4000), "strengths": lines, "gaps": lines, "skills": lines,
    "recruiter_name": string(maxLength=4000), "recruiter_contact": string(maxLength=4000),
    "next_action": string(maxLength=4000), "next_action_at": date,
    "waiting_on": string(enum=["", "agent", "recruiter", "you"]), "needs_you": {"type": "boolean", "default": False},
    "interview_at": date, "interview_url": url,
}
identity = {"source_key": string(minLength=1, maxLength=300, description="Stable unique key per profile. Reusing with changed create payload is a conflict."), "profile": string(maxLength=64, pattern="^[a-z0-9_-]+$", default="sergey")}
job_fields = {"id": {"type": "integer", "minimum": 1, "readOnly": True}, **identity, **fields,
    "version": {"type": "integer", "minimum": 1, "readOnly": True},
    **{k: {**date, "readOnly": True} for k in ["created_at", "updated_at", "last_checked_at"]}}
event_fields = {
    "source_key": string(minLength=1,maxLength=300, description="Stable unique key per job. Retry the identical event; never resend an external message to retry logging."),
    "kind": string(enum=["note", "discovered", "application", "message", "follow_up", "interview", "status", "check", "document"]),
    "summary": string(minLength=1,maxLength=2000), "body": string(maxLength=20000), "actor": string(minLength=1,maxLength=200),
    "channel": string(enum=["", "email", "linkedin", "telegram", "ats", "calendar", "manual", "system", "other"]),
    "direction": string(enum=["", "inbound", "outbound", "internal"]),
    **{k:string(maxLength=1000) for k in ["contact", "account", "thread_id", "external_id"]},
    "source_url": url, "verification": string(enum=["recorded", "confirmed", "uncertain"],default="recorded",description="Caller assertion only; the server does not prove an external action."),
    "happened_at": {**date,"description":"When it happened, defaults to receipt time if omitted. Retrying an omitted timestamp is safe."},
    "stage": string(enum=["", *stages],description="Optional stage applied atomically with this event. A recorded state, not send authorization.")
}
schemas = {
    "JobCreate": obj({**identity,**fields,"profile":string(enum=["sergey","polina"],default="sergey")}, ["source_key","company","title"]),
    "JobPatch": obj(fields), "Job": obj(job_fields, list(job_fields)),
    "EventCreate": obj(event_fields,["source_key","kind","summary","actor"]),
    "Event": obj({**event_fields,"id":{"type":"integer"},"job_id":{"type":"integer"},"recorded_at":date}),
    "Error": obj({"error":obj({"code":string(),"message":string()},["code","message"])},["error"])
}
for name,item in [("JobPage","Job"),("EventPage","Event")]: schemas[name]=obj({"items":{"type":"array","items":ref(item)},"next_cursor":string(description="Empty when exhausted; otherwise use as cursor in the next request.")},["items","next_cursor"])
count = {"type":"integer","minimum":0}
schemas["StatsBreakdown"] = obj({"label":string(),"count":count},["label","count"])
schemas["StatsDay"] = obj({"date":string(format="date"),"jobs":count,"events":count},["date","jobs","events"])
stats_fields = {name:count for name in ["total","saved","needs_you","active","offers","hired","rejected","archived"]}
stats_fields.update({name:{"type":"array","items":ref("StatsBreakdown")} for name in ["profiles","remotes","employment","waiting_on"]})
stats_fields.update({"days":{"type":"array","minItems":30,"maxItems":30,"items":ref("StatsDay")},"from":string(format="date"),"to":string(format="date")})
schemas["Stats"] = {**obj(stats_fields,list(stats_fields)),"description":"Full-profile overview, independent of list filters or pagination. Active = applied/talking/interview; needs_you excludes hired/rejected/archived. Total includes archived. Profiles breakdown always includes both active profiles workspace-wide. Days cover the last 30 UTC dates, jobs by creation and events by occurrence."}
def response(description,schema,etag=False,replay=False):
    value={"description":description,"content":{"application/json":{"schema":ref(schema)}}}
    if etag: value.setdefault("headers",{})["ETag"]={"schema":string(),"description":"Quoted version, e.g. \"3\"; use in If-Match."}
    if replay: value.setdefault("headers",{})["Idempotent-Replay"]={"schema":string(enum=["true"]),"description":"Present on an identical replay."}
    return value
errors={str(code):response(desc,"Error") for code,desc in [(400,"Invalid input/query"),(401,"Bearer key missing or invalid"),(404,"Not found"),(405,"Method not allowed"),(409,"Source-key payload conflict or stale version"),(413,"Payload over 256 KiB"),(415,"Expected application/json"),(428,"If-Match required"),(500,"Internal error; no database details disclosed")]}
def param(name,schema,where="query",required=False):return {"name":name,"in":where,"required":required,"schema":schema}
paging=[param("limit",{"type":"integer","minimum":1,"maximum":100,"default":50}),param("cursor",string(pattern="^[1-9][0-9]*$"))]
jobid=param("id",{"type":"integer","minimum":1},"path",True)
def operation(op,summary,schema,parameters=None,body=None,created=False,etag=False):
    result={"operationId":op,"summary":summary,"responses":{"200":response("Success / exact replay" if created else "Success",schema,etag,created),**errors}}
    if parameters: result["parameters"]=parameters
    if body:result["requestBody"]={"required":True,"content":{"application/json":{"schema":ref(body)}}}
    if created:result["responses"]["201"]=response("Created",schema,etag)
    return result
spec={"openapi":"3.1.0","info":{"title":"JobHub Agent API","version":"1.0.0","description":"Private job management and append-only timeline. No outbound execution. Jobs and events require stable source keys. All requests require bearer auth. Times are RFC3339. Job lists and event lists use newest-record-ID-first keyset pagination. A new event increments its job version. Only check events advance last_checked_at, using the actual check occurrence time. Rotating the key and restarting revokes browser sessions."},"servers":[{"url":"/api/v1"}],"security":[{"bearerAuth":[]}],"components":{"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer","description":"JOBHUB_API_KEY from private local environment; never use URL query keys."}},"schemas":schemas},"paths":{
    "/jobs":{"get":operation("listJobs","List and filter jobs","JobPage",paging+[param("profile",string()),param("stage",string(enum=[*stages,"active"],description="active matches applied, talking, interview; it is a filter, not a writable stage.")),param("remote",string(enum=["unknown","remote","hybrid","onsite"])),param("q",string()),param("needs_you",{"type":"boolean","description":"Only open jobs needing attention; excludes hired/rejected/archived."})]),"post":operation("createJob","Create job or replay original create payload","Job",body="JobCreate",created=True,etag=True)},
    "/stats":{"get":operation("getStats","Read profile overview and 30-day activity","Stats",[param("profile",string(enum=["sergey","polina"],description="Omit to aggregate both active profiles."))])},
    "/jobs/{id}":{"get":operation("getJob","Read complete job and current version","Job",[jobid],etag=True),"patch":operation("patchJob","Merge editable fields with optimistic concurrency","Job",[jobid,param("If-Match",string(pattern='^"[1-9][0-9]*"$'),"header",True)],body="JobPatch",etag=True)},
    "/jobs/{id}/events":{"get":operation("listEvents","List append-only timeline records","EventPage",[jobid,*paging]),"post":operation("appendEvent","Append one event, optionally changing stage atomically","Event",[jobid],body="EventCreate",created=True)},
    "/openapi.json":{"get":{"operationId":"openAPISpec","summary":"Read this machine-readable API contract","responses":{"200":{"description":"OpenAPI 3.1 specification","content":{"application/json":{"schema":{"type":"object"}}}},"401":errors["401"]}}}
}}
target=Path(__file__).resolve().parents[1]/"internal/workspace/openapi.json"
target.write_text(json.dumps(spec,indent=2)+"\n")
print(f"Generated {target.relative_to(target.parents[2])}")
