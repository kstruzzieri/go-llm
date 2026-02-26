import math


def calculate_area(radius: float) -> float:
    """Calculate the area of a circle."""
    return math.pi * radius ** 2


def calculate_circumference(radius: float) -> float:
    """Calculate the circumference of a circle."""
    return 2 * math.pi * radius


class Circle:
    def __init__(self, radius: float):
        self.radius = radius

    def area(self) -> float:
        return calculate_area(self.radius)

    def circumference(self) -> float:
        return calculate_circumference(self.radius)


def main():
    c = Circle(5.0)
    print(f"Area: {c.area()}")
    print(f"Circumference: {c.circumference()}")


if __name__ == "__main__":
    main()
