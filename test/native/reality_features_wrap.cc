// Test-only linker wrapper for exercising TlsStreamAttempt without changing the
// published native library's disabled-by-default Happy Eyeballs V3 policy.
#include <string>

extern "C" bool RealityTestRealInit(const std::string&, const std::string&)
    asm("__real__ZN4base11FeatureList12InitInstanceERKNSt4__Cr12basic_stringIcNS1_11char_traitsIcEENS1_9allocatorIcEEEES9_");

extern "C" bool RealityTestWrappedInit(const std::string&, const std::string&)
    asm("__wrap__ZN4base11FeatureList12InitInstanceERKNSt4__Cr12basic_stringIcNS1_11char_traitsIcEENS1_9allocatorIcEEEES9_");

extern "C" bool RealityTestWrappedInit(const std::string& enabled,
                                      const std::string& disabled) {
  return RealityTestRealInit(enabled + ",HappyEyeballsV3", disabled);
}
