# Golden packets: Deep Symmetry CDJ-2000NXS captures

Each `.hex` file holds one UDP payload, named `<dst-port>-<kind>-<length>.hex`.
The first line is a comment naming the source capture and the endpoints.

The packets were cut from the hardware captures in Deep Symmetry's
[dysentery](https://github.com/Deep-Symmetry/dysentery/tree/main/doc/assets/captures)
repository: two CDJ-2000nexus players on firmware 1.44, link-local addressing,
recorded 2026-07-29. They are raw wire bytes from real hardware, used here as
test data only.
