-- Create returns a table lacking the interface's methods (only Foo).
function Create()
  return { Foo = function(_, name) return name end }
end
