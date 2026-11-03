-- The entrypoint fails while it runs, before Create.
local config = nil
local port = config.port

function Create()
  return { port = port }
end
