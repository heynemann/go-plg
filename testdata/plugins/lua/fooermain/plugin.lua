-- The Lua counterpart of testdata/plugins/go/fooermain: a second Fooer, a plain table of functions, whose Scalars
-- returns its arguments unchanged.
function Create()
  return {
    Foo = function(_, name) return "main says hello, " .. name:match("^%s*(.-)%s*$") end,
    Count = function() return 0 end,
    Fail = function() end,
    Boil = function() return { false, 0 } end,
    Scalars = function(_, ...) return { ... } end,
    Spin = function(_, n) return n end,
  }
end
