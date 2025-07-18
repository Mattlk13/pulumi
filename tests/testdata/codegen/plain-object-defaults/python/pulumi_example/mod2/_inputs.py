# coding=utf-8
# *** WARNING: this file was generated by test. ***
# *** Do not edit by hand unless you're certain you know what you are doing! ***

import builtins as _builtins
import warnings
import sys
import pulumi
import pulumi.runtime
from typing import Any, Mapping, Optional, Sequence, Union, overload
if sys.version_info >= (3, 11):
    from typing import NotRequired, TypedDict, TypeAlias
else:
    from typing_extensions import NotRequired, TypedDict, TypeAlias
from .. import _utilities
from .. import mod1 as _mod1

__all__ = [
    'TypArgs',
    'TypArgsDict',
]

MYPY = False

if not MYPY:
    class TypArgsDict(TypedDict):
        """
        A test for namespaces (mod 2)
        """
        mod1: NotRequired[pulumi.Input['_mod1.TypArgsDict']]
        val: NotRequired[pulumi.Input[_builtins.str]]
elif False:
    TypArgsDict: TypeAlias = Mapping[str, Any]

@pulumi.input_type
class TypArgs:
    def __init__(__self__, *,
                 mod1: Optional[pulumi.Input['_mod1.TypArgs']] = None,
                 val: Optional[pulumi.Input[_builtins.str]] = None):
        """
        A test for namespaces (mod 2)
        """
        if mod1 is not None:
            pulumi.set(__self__, "mod1", mod1)
        if val is None:
            val = 'mod2'
        if val is not None:
            pulumi.set(__self__, "val", val)

    @_builtins.property
    @pulumi.getter
    def mod1(self) -> Optional[pulumi.Input['_mod1.TypArgs']]:
        return pulumi.get(self, "mod1")

    @mod1.setter
    def mod1(self, value: Optional[pulumi.Input['_mod1.TypArgs']]):
        pulumi.set(self, "mod1", value)

    @_builtins.property
    @pulumi.getter
    def val(self) -> Optional[pulumi.Input[_builtins.str]]:
        return pulumi.get(self, "val")

    @val.setter
    def val(self, value: Optional[pulumi.Input[_builtins.str]]):
        pulumi.set(self, "val", value)


