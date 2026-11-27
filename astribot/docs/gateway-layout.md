# Astribot Gateway layout

The shared Gateway registers routes and provides authentication, response formatting and clients. Astribot's cluster provisioning and SSH join workflows live in `apps/rlark/astribot/provisioning/workflow`. Station observations and robot controls live in `stations`; station network and admission contracts live in `stationidentity`. Storage declaration, validation and rendering live in `storageconfig`, and registry image metadata lives in `registry`.

The extraction retains the established API routes and does not require a data migration. Gateway and workflow regression tests exercise the same entry points and response contracts.
