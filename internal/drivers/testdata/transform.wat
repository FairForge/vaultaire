(module
  (memory (export "memory") 1)

  (func $malloc (param $size i32) (result i32)
    i32.const 0
  )

  (func $transform (param $ptr i32) (param $len i32) (result i32 i32)
    local.get $ptr
    local.get $len
  )

  (export "malloc" (func $malloc))
  (export "transform" (func $transform))
)
