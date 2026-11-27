# Astribot deployment and storage

The site Addon catalog lives in `apps/rlark/astribot/addoncatalog/catalog/`. Gateway, Station Controller packaging and release archives read both the upstream and Astribot catalogs. Published Addon names and versions retain their identity across directory moves.

Concurrent edits to a storage declaration can cause an update or delete to return `409 RESOURCE_CONFLICT`. Reload the declaration before retrying.
