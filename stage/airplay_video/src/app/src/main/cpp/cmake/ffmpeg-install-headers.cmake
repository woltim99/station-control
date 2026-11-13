# Copy the public FFmpeg headers without going through a shell command line:
# the generated `install` recipe can exceed the Windows command-line limit.
file(GLOB _avutil_headers "${SRC}/libavutil/*.h")
file(GLOB _avcodec_headers "${SRC}/libavcodec/*.h")

file(MAKE_DIRECTORY "${DST}/libavutil" "${DST}/libavcodec")
file(COPY ${_avutil_headers} DESTINATION "${DST}/libavutil")
file(COPY ${_avcodec_headers} DESTINATION "${DST}/libavcodec")

foreach(_generated avconfig.h ffversion.h)
    if(EXISTS "${BUILD}/libavutil/${_generated}")
        file(COPY "${BUILD}/libavutil/${_generated}" DESTINATION "${DST}/libavutil")
    endif()
endforeach()
