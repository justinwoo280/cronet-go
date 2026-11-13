// Regression for MIPS GOT16 alias materialization in libc++ override detection.
// Keep the checks explicit: the native archive is built with NDEBUG.
#include <cstdint>
#include <cstdio>
#include <new>

#include "src/include/overridable_function.h"

OVERRIDABLE_FUNCTION [[gnu::noinline]] int cronet_default_probe() { return 1; }
OVERRIDABLE_FUNCTION [[gnu::noinline]] int cronet_override_probe() { return 2; }

int main() {
  if (std::__is_function_overridden<int(), &cronet_default_probe>() ||
      !std::__is_function_overridden<int(), &cronet_override_probe>() ||
      cronet_default_probe() != 1 || cronet_override_probe() != 3) {
    std::puts("incorrect weak/strong function override detection");
    return 1;
  }
  if (std::__is_function_overridden<void*(std::size_t), &operator new>() ||
      std::__is_function_overridden<void*(std::size_t), &operator new[]>()) {
    std::puts("default allocation operators were incorrectly identified as overrides");
    return 2;
  }
  void* scalar = ::operator new(48, std::nothrow);
  void* array = ::operator new[](48, std::nothrow);
  void* aligned = ::operator new(64, std::align_val_t(64), std::nothrow);
  void* aligned_array = ::operator new[](64, std::align_val_t(64), std::nothrow);
  if (!scalar || !array || !aligned || !aligned_array ||
      reinterpret_cast<std::uintptr_t>(aligned) % 64 != 0 ||
      reinterpret_cast<std::uintptr_t>(aligned_array) % 64 != 0) {
    return 3;
  }
  ::operator delete(scalar);
  ::operator delete[](array);
  ::operator delete(aligned, std::align_val_t(64));
  ::operator delete[](aligned_array, std::align_val_t(64));
  std::puts("Override detection and all four nothrow allocation forms passed");
}
