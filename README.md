# lastfm-scrobbles
server + client for loading and streaming playing tracks on lastfm


# frontend
this is a riot project with 99% of things crammed in a big component (scrobbbl-container.riot) that could probably cleaned up. not to mention the awful css. most of the effort was not duping ws updates. changed this from streaming from backend websocket (see below) to 
hitting api directly with key in client and some minimal styling.


# backend.
this is a simple go server with `/streaming` endpoint that will upgrade ws and send tracks over it. it needs caching by username and maybe a different endpoint for initial history to simplif multiple clients. not currently used by frontend

