# Source: classic/forge/forge/command/decorator.py
#         classic/forge/forge/command/command.py
#         classic/forge/forge/command/parameter.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/command/decorator.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/command/command.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/command/parameter.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls the three pieces of upstream's Command system into one
# annotated reading for s02 of learn-AutoGPT. It shows how Python's
# `@command` decorator turns a function into an auto-registerable
# Command object — the implicit alternative to our Go Registry.
#
# Lines marked [→ sNN] indicate which Go session in this repo teaches the
# corresponding upstream concept. Generic typing (ParamSpec/TypeVar) and
# import boilerplate are stripped where they distract from the structure.


# ─────────────────────────────────────────────────────────────────────────
# command/parameter.py · the parameter schema container
# ─────────────────────────────────────────────────────────────────────────

class CommandParameter(BaseModel):
    """A single named parameter for a command.

    `spec` is a JSONSchema fragment describing the parameter's type,
    description, required flag, and so on. Pydantic's BaseModel gives
    us validation + serialization for free.   [→ s02: ToolSchema.InputSchema]
    """
    name: str
    spec: JSONSchema

    def __repr__(self):
        return "CommandParameter('%s', '%s', '%s', %s)" % (
            self.name,
            self.spec.type,
            self.spec.description,
            self.spec.required,
        )


# ─────────────────────────────────────────────────────────────────────────
# command/command.py · the Command class
# ─────────────────────────────────────────────────────────────────────────

class Command:
    """A class representing a command.

    Attributes:
        names (list[str]): aliases for the command (first one is canonical)
        description (str): brief description shown to the LLM
        method (Callable): the actual handler function
        parameters (list[CommandParameter]): parameter schema
    """

    def __init__(
        self,
        names: list[str],
        description: str,
        method: Callable,
        parameters: list[CommandParameter],
    ):
        # Validate: parameter names declared by the decorator must match
        # the function signature. This is the one static check Python can
        # do at import time — mismatch raises ValueError on module load.
        # [→ s02: Registry.Register validates name is non-empty;
        #         Tool.Execute does runtime arg-shape checks via
        #         requireString/requireNumber]
        if not self._parameters_match(method, parameters):
            raise ValueError(
                f"Command {names[0]} has different parameters than provided schema"
            )
        self.names = names
        self.description = description
        self.method = method
        self.parameters = parameters

    @property
    def is_async(self) -> bool:
        return inspect.iscoroutinefunction(self.method)

    def _parameters_match(
        self, func: Callable, parameters: list[CommandParameter]
    ) -> bool:
        signature = inspect.signature(func)
        func_param_names = [
            param.name
            for param in signature.parameters.values()
            if param.name != "self"
        ]
        names = [param.name for param in parameters]
        return sorted(func_param_names) == sorted(names)

    def __call__(self, *args, **kwargs):
        return self.method(*args, **kwargs)

    def __str__(self) -> str:
        params = [
            f"{param.name}: "
            + ("%s" if param.spec.required else "Optional[%s]")
            % (param.spec.type.value if param.spec.type else "Any")
            for param in self.parameters
        ]
        return (
            f"{self.names[0]}: {self.description.rstrip('.')}. "
            f"Params: ({', '.join(params)})"
        )

    def __get__(self, instance, owner):
        # Descriptor protocol — when a Command attached to a class is
        # accessed via an instance, auto-bind self. Required so that
        # @command-decorated methods on a Component can be invoked as
        # normal bound methods.  [→ s08: components hold their own
        # Tool methods; Go interface satisfaction handles binding
        # without descriptors]
        if instance is None:
            return self
        return Command(
            self.names,
            self.description,
            self.method.__get__(instance, owner),
            self.parameters,
        )


# ─────────────────────────────────────────────────────────────────────────
# command/decorator.py · the @command decorator
# ─────────────────────────────────────────────────────────────────────────

def command(
    names: list[str] = [],
    description: Optional[str] = None,
    parameters: dict[str, JSONSchema] = {},
):
    """
    The command decorator wraps a function as a Command. This is the
    "implicit registration" style: importing the module that defines the
    function causes the function to be REPLACED by a Command instance,
    and any CommandProvider that scans the module picks it up.
    [→ s02: Go Registry.Register is the explicit alternative]

    Args:
        names: aliases. If empty, func.__name__ is used.
        description: shown to the LLM. If empty, the docstring's first
            paragraph is used (with whitespace collapsed).
        parameters: dict of param-name → JSONSchema spec.
    """
    def decorator(func):
        doc = func.__doc__ or ""
        # If names is not provided, use the function name.
        # [→ s02: Tool.Schema().Name is the single canonical name; aliases
        #         are deferred to Appendix B as exercise material]
        command_names = names or [func.__name__]

        # If description is not provided, use the docstring's first paragraph.
        # [→ s02: Tool.Schema().Description is set explicitly per tool;
        #         no docstring magic in Go]
        if not (command_description := description):
            if not func.__doc__:
                raise ValueError(
                    "Description is required if function has no docstring"
                )
            command_description = re.sub(r"\s+", " ", doc.split("\n\n")[0].strip())

        # Wrap each {name: schema} into a CommandParameter object.
        # [→ s02: ToolSchema.InputSchema is a flat JSONSchema map; we don't
        #         model individual parameters as separate objects]
        typed_parameters = [
            CommandParameter(name=param_name, spec=spec)
            for param_name, spec in parameters.items()
        ]

        # Replace func with Command. After import, `my_func` at module
        # scope IS now a Command instance — that's how @command achieves
        # auto-registration without an explicit Register call.
        # [→ s02: in Go, main.go writes `reg.Register(NewEchoTool())`
        #         explicitly, making the dependency edge grep-visible]
        return Command(
            names=command_names,
            description=command_description,
            method=func,
            parameters=typed_parameters,
        )

    return decorator


# ─────────────────────────────────────────────────────────────────────────
# Reading map — which session of learn-AutoGPT teaches each upstream symbol
# ─────────────────────────────────────────────────────────────────────────
#
# @command decorator               → s02 (this is the central comparison —
#                                          implicit decorator vs explicit Register)
# Command class                    → s02 (our Tool interface plays this role)
# CommandParameter                 → s02 (we use a flat ToolSchema.InputSchema
#                                          instead of per-parameter objects)
# Command._parameters_match        → s02 (we validate at runtime in Tool.Execute,
#                                          not at registration time)
# Command.names (multi-alias)      → Appendix B exercise (not in any session)
# Command.__get__ descriptor       → s08 (component methods; Go interface
#                                          satisfaction handles binding cleanly)
# Command.is_async                 → out of scope (Go uses ctx.Context everywhere
#                                          rather than mixing sync/async signatures)
# CommandProvider.get_commands     → s08 (component-as-tool-source — components
#                                          emit tools INTO the registry)
# AppConfig.disabled_commands      → s07 (permissions covers this with a more
#                                          general Allow/Deny pattern system)
# JSONSchema (used by spec)        → s04 (prompt strategy renders schemas into
#                                          system prompt in a readable form)
