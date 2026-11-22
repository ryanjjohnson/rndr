#!/bin/sh
# Builds ./rndr: a single self-running file (shell stub + jar) that needs only a JVM (8+).
set -e
cd "$(dirname "$0")"
rm -rf build
mkdir -p build/classes
javac -nowarn --release 8 -d build/classes Rndr.java 2>/dev/null || javac -nowarn -d build/classes Rndr.java
jar cfe build/rndr.jar Rndr -C build/classes .
{
    printf '#!/bin/sh\nexec java $RNDR_JAVA_OPTS -jar "$0" "$@"\n'
    cat build/rndr.jar
} > rndr
chmod +x rndr
rm -rf build
echo "built $(pwd)/rndr"
