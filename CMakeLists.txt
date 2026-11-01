# bfbc - ReXGlue Recompiled Project
#
# SDK boilerplate lives in generated/rexglue.cmake.

cmake_minimum_required(VERSION 3.25)
project(bfbc LANGUAGES CXX RC)

set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

include(generated/rexglue.cmake)

# Sources
set(BFBC_SOURCES
    src/main.cpp
    src/mouse_look.cpp
    src/bfbc.rc
)

if(WIN32)
    add_executable(bfbc WIN32 ${BFBC_SOURCES})
else()
    add_executable(bfbc ${BFBC_SOURCES})
endif()

rexglue_setup_target(bfbc GPU_PLUGINS xenos)
