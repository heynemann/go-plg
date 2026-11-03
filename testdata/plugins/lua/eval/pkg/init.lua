-- require("pkg") finds this file; it requires its submodule by its full name.
local sub = require("pkg.sub.leaf")
return { name = "pkg", leaf = sub.name, args = { ... } }
