-- A one-file Lua plugin of an FS storage, next to Go ones.
function Create()
  return {
    Foo = function(_, name) return "hello, " .. name end,
    Count = function() return 0 end,
    Fail = function() end,
    Boil = function() return { false, 0 } end,
    Scalars = function(_, ...) return { ... } end,
    Spin = function(_, n) return n end,
  }
end
