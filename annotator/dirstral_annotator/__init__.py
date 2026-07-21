"""dirstral-annotator: external recognition pipeline for the dir2mcp video
intelligence pilot. Emits time-coded annotation sidecars (dirstral-spec
design 0004) that dir2mcp indexes into searchable, citable moments.

Deliberately NOT part of the dir2mcp Go core: dir2mcp indexes what this
package publishes; it never runs it (design 0004 §5).
"""

__version__ = "0.1.0"
